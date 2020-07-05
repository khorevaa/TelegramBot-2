package telegram

import (
	"Teaching/github.com/pkg/errors"
	fresh "TelegramBot/Fresh"
	n "TelegramBot/Net"
	"TelegramBot/Redis"
	"bytes"
	"encoding/json"
	"fmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/sirupsen/logrus"
	"net/http"
	"reflect"
	"time"
)


// Информация по заявкам хранится в Radis
// Структура хранения такая
//
// - инфо по заявкам СУИ:
// 			key - TicketID <идентификатор тикета>, value (hash) - TicketNumberm: <номер тикета>, ArticleID: <хз что, но это возвращатся в ответ при создании заявки>
// - Связь обновления и тикеты: key - <ref задания на обновления в агенте>, value - TicketID (один ко многим)
// - список активных тикетов СУИ, key - activeTickets
//
// Команды redis: https://redis.io/commands

type Ticket struct {
	Title string
	Type string
	Queue string
	State string
	Priority string
	Service string
	SLA string
	Owner string
	Responsible string
	CustomerUser string
}
type Article struct {
	Subject string
	Body string
	ContentType string
}

type RequestDTO struct {
	UserLogin string
	Password string
	Ticket *Ticket
	Article *Article
	DynamicField []struct{
		Name string
		Value string
	}
}

type TicketInfo struct {
	ArticleID string  `json:"ArticleID"`
	TicketNumber string  `json:"TicketNumber"`
	TicketID string  `json:"TicketID"`
}

type SUI struct {
	BaseTask
	GetListUpdateState

	respData *TicketInfo
	fresh    *fresh.Fresh
	agent    string
	redis *redis.Redis
}


//////////////////////////////////////////////////////////////////////////////////////////////////////

func (this *SUI) Initialise(bot *tgbotapi.BotAPI, update *tgbotapi.Update, finish func()) ITask {
	this.BaseTask.Initialise(bot, update, finish)
	this.EndTask[reflect.TypeOf(this).String()] = []func(){finish}
	this.fresh = new(fresh.Fresh)
	this.redis, _ = new(redis.Redis).Create(Confs.Redis)

	agentStep := new(step).Construct("Выберите агент сервиса для получения списка заданий обновления", "choseAgent", this, ButtonCancel|ButtonBack, 2)
	for _, conffresh := range Confs.FreshConf {
		Name := conffresh.Name // Обязательно через переменную, нужно для замыкания
		Alias := conffresh.Alias
		this.fresh.Conf = conffresh

		agentStep.appendButton(Alias, func() {
			this.ChoseAgent(Name)
			this.agent = Alias
			this.next("")
		})
	}
	agentStep.reverseButton()

	this.steps = []IStep{
		new(step).Construct("Что прикажете?", "start", this, ButtonCancel, 2).
			appendButton("Создать", func() {
							this.next("")
						}).
			appendButton("Завершить", func() {
							this.gotoByName("endTicket", "")
						}),
		agentStep,
		new(step).Construct("", "createTicket", this, ButtonCancel|ButtonBack, 3).
			whenGoing(func(thisStep IStep) {
				// исключаем те задания агента по которым уже создана заявка в СУИ
				for i := len(this.updateTask)-1; i >= 0; i-- {
					if this.redis.KeyExists(this.updateTask[i].UUID) {
						this.updateTask = append(this.updateTask[:i], this.updateTask[i+1:]...)
					}
				}

				if len(this.updateTask) == 0 {
					thisStep.(*step).Buttons = []map[string]interface{}{}
					thisStep.(*step).txt = "Активных заданий на обновления не найдено"
				} else  {
					thisStep.(*step).txt = fmt.Sprintf("Запланировано %v заданий на обновления, создать задачу в СУИ?", len(this.updateTask))
				}
				//thisStep.reverseButton()
			}).appendButton("Да", func() {
				if err := this.createTask(); err == nil {
					this.gotoByName("end", fmt.Sprintf("Создана заявка с номером %q", this.respData.TicketNumber))

					go this.deferredExecution(time.Hour *8, func() {
						logrus.WithField("task", this.GetDescription()).
							WithField("TicketData", this.respData).
							Info("Удаленмие заявки в СУИ по таймауту")

						if this.redis.Count("activeTickets") == 0 {
							return
						}

						this.bot.Send(tgbotapi.NewMessage(this.ChatID, fmt.Sprintf("Завершение заявки %q в СУИ по таймауту", this.respData.TicketNumber)))
						this.completeTask(this.respData.TicketID)
						this.innerFinish()
					})
				}  else {
					logrus.WithError(err).Error()
					this.gotoByName("end", "При создании таска в СУИ произошла ошибка")
				}
			}),
		new(step).Construct("Завершить", "endTicket", this, ButtonCancel, 2).whenGoing(func(thisStep IStep) {
			tickets := this.getTickets()
			if len(tickets) == 0 {
				thisStep.(*step).txt  = "Нет активных заявок СУИ"
			} else {
				thisStep.(*step).txt = "Завершить следующие заявки в СУИ:\n"
				for _, t := range tickets {
					thisStep.(*step).txt  += t.TicketNumber + "\n"
				}
				thisStep.appendButton("Да", func() {
					for _, v := range tickets {
						this.completeTask(v.TicketID)
					}
					this.gotoByName("end", "Готоводело")
					this.innerFinish()
				})
			}
		}),
		new(step).Construct("", "end", this, 0, 1),
	}

	this.AppendDescription(this.name)
	return this
}

func (this *SUI) Start() {
	logrus.WithField("description", this.GetDescription()).Debug("Start")
	this.steps[this.currentStep].invoke(&this.BaseTask)
}

func (this *SUI) InfoWrapper(task ITask) {
	this.info = "ℹ Данным заданиям можно создать тески в СУИ по запланированым работам связанным с обновлением системы."
	this.BaseTask.InfoWrapper(task)
}

func (this *SUI) createTask() error {
	logrus.WithField("task", this.GetDescription()).Debug("Создаем задачу в СУИ")
	if len(this.updateTask) == 0 {
		return errors.New("Нет данных по обновлениям")
	}

	basesFiltr := []string{}
	for _, v := range this.updateTask {
		basesFiltr = append(basesFiltr, v.Base)
	}

	var bases = []*Bases{}
	var groupByConf = map[string][]*Bases{}
	if err := this.JsonUnmarshal(this.fresh.GetDatabase(basesFiltr), &bases); err != nil {
		return err
	} else {
		for _, v := range bases {
			if _, ok := groupByConf[v.Conf]; !ok {
				groupByConf[v.Conf] = []*Bases{}
			}
			groupByConf[v.Conf] = append(groupByConf[v.Conf], v)
		}
	}

	TaskBody := fmt.Sprintf("Обновление контура %q\n\nКонфигурации:\n", this.agent)
	for k, v := range groupByConf {
		TaskBody += fmt.Sprintf("\t- %v\n", k)
		for _, base := range v {
			TaskBody += fmt.Sprintf("\t\t* %v (%v)\n", base.Caption, base.Name)
		}
	}


	body := RequestDTO{
		UserLogin: Confs.SUI.User,
		Password:  Confs.SUI.Pass,
		Ticket: &Ticket{
			Title: "Плановые обновления конфигурации ЕИС УФХД",
			Type: "Запрос на обслуживание",
			Queue: "УПРАВЛЕНИЯ ФИНАНСОВО-ХОЗЯЙСТВЕННОЙ ДЕЯТЕЛЬНОСТИ (УФХД)",
			State: "В работе",
			Priority: "Приоритет 4 – низкий",
			Service: "7.УФХД: Обслуживание системы ",
			SLA: "7.УФХД: SLA (низкий приоритет)",
			Owner: "3lpufhdnparma",
			Responsible: "3lpufhdnparma",
			CustomerUser: "api_ufhd",
		},
		Article: &Article{
			Subject: "Плановые обновления конфигурации ЕИС УФХД",
			Body: TaskBody,
			ContentType: "text/plain; charset=utf8",
		},
		DynamicField: []struct{
						Name string
						Value string
					}{
						{
							"TicketSource", "Web",
						},
						{
							"ProcessManagementProcessID", "Process-74a8bd3dd6515fb7d1faf68aa5d2d1d0",
						},
						{
							 "ProcessManagementActivityID","Activity-932c4c75e80f46f35ebc4c1e3e387915",
						},
					},
	}
	jsonResp, err := this.sendHTTPRequest(http.MethodPost, fmt.Sprintf("%v/Ticket", Confs.SUI.URL), body)
	if err == nil {
		err = json.Unmarshal([]byte(jsonResp), &this.respData)
		this.addRedis()
	} else {
		logrus.WithError(err).Error("Произошла ошибка при отпраке запроса в СУИ")
	}

	return err
}

func (this *SUI) completeTask(TicketID string) {
	if TicketID == "" {
		return
	}
	logrus.WithField("task", this.GetDescription()).WithField("TicketData", this.respData).Debug("Удаляем задачу в СУИ")
	if !this.checkState(TicketID) {
		logrus.WithField("task", this.GetDescription()).WithField("TicketData", this.respData).Debug("Заявка уже закрыта")
		return
	}

	// что б не описывать структуру, решил так
	body := map[string]interface{}{
		"UserLogin": Confs.SUI.User,
		"Password": Confs.SUI.Pass,
		"TicketID": TicketID,
		"Ticket": map[string]interface{} {
			"State": "Решение предоставлено",
			"PendingTime": map[string]interface{} {
				"Diff": "86400",
			},
		},
		"Article": map[string]interface{} {
			"Subject": "Закрытие тикета",
			"Body": "Базы обновлены",
			"ContentType": "text/plain; charset=utf8",
		},
	}
	_, err := this.sendHTTPRequest(http.MethodPatch, fmt.Sprintf("%v/Ticket/%v", Confs.SUI.URL, TicketID), body)
	if err != nil {
		logrus.WithError(err).Error("Произошла ошибка при отпраке запроса в СУИ")
		this.bot.Send(tgbotapi.NewMessage(this.ChatID, fmt.Sprintf("Произошла ошибка при отпраке запроса в СУИ:\n%v", err)))
	}

	// удаляем из списка активных
	this.redis.DeleteItems("activeTickets", TicketID)
}

func (this *SUI) getTickets() []*TicketInfo  {
	result := []*TicketInfo{}
	activeTickets := this.redis.Items("activeTickets")
	for _, v := range activeTickets {
		ticket := this.redis.StringMap(v)
		result = append(result, &TicketInfo{
			ArticleID:    ticket["ArticleID"],
			TicketNumber: ticket["TicketNumber"],
			TicketID:     v,
		})
	}

	return  result
}

func (this *SUI) deferredExecution(delay time.Duration, f func())  {
	timeout := time.NewTicker(delay)
	<-timeout.C

	f()
	timeout.Stop()
}

func (this *SUI) checkState(TicketID string) bool {
	jsonTxt, _ := this.sendHTTPRequest(http.MethodGet, fmt.Sprintf("%v/TicketList?UserLogin=%v&Password=%v&TicketID=%v", Confs.SUI.URL, Confs.SUI.User, Confs.SUI.Pass, TicketID), nil)

	data := map[string]interface{}{}
	if err := json.Unmarshal([]byte(jsonTxt), &data); err != nil {
		return true
	}

	if v, ok := data["Ticket"]; ok {
		for _, item := range v.([]interface{}) {
			if item.(map[string]interface{})["TicketID"] != TicketID {
				continue
			}

			return item.(map[string]interface{})["State"] != "Решение предоставлено"
		}

	} else {
		return true
	}

	return true
}

func (this *SUI) sendHTTPRequest(method, url string, dto interface{}) (string, error) {
	logrus.WithField("dto", dto).WithField("url", url).Debug("Отправка запроса в СУИ")

	netU := new(n.NetUtility).Construct(url, "", "")
	if dto != nil {
		if body, err := json.Marshal(dto); err == nil {
			netU.Body = bytes.NewReader(body)
		} else {
			return "", err
		}
	}

	return netU.CallHTTP(method, time.Minute, nil)
}

func (this *SUI) addRedis()  {
	if this.respData == nil {
		return
	}

	this.redis.Begin()
	logrus.WithField("data", this.respData).Debug("Добавляем данные по заявке в redis")

	// Данные по созданной заявке
	data := map[string]string{
		"TicketNumber": this.respData.TicketNumber,
		"ArticleID": this.respData.ArticleID,
	}
	this.redis.SetMap(this.respData.TicketID, data)

	// связь обновление - тикет
	for _, v := range this.updateTask {
		this.redis.AppendItems(v.UUID, this.respData.TicketID)
	}

	// добавляем в список активных тикетов
	this.redis.AppendItems("activeTickets", this.respData.TicketID)
	this.redis.Commit()
}