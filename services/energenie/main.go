// Service to communicate with energenie sockets. This can both receive and
// transmit events.
package energenie

import (
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/barnybug/ener314"
	"github.com/barnybug/gohome/pubsub"
	"github.com/barnybug/gohome/services"
	"github.com/barnybug/gohome/util"
)

type Action string

const (
	TargetTemperature Action = "TargetTemperature"
	Identify          Action = "Identify"
	Exercise          Action = "Exercise"
	Diagnostics       Action = "Diagnostics"
	Voltage           Action = "Voltage"
	ValveState        Action = "ValveState"
	PowerMode         Action = "Powermode"
)

type SensorRequest struct {
	Action      Action
	Temperature float64
	ValveState  ener314.ValveState
	Mode        ener314.PowerMode
	Repeat      int
}

type SensorRequestQueue []SensorRequest

func (q SensorRequestQueue) Append(s SensorRequest) SensorRequestQueue {
	// dedup
	var ret SensorRequestQueue
	for _, i := range q {
		if i.Action != s.Action {
			ret = append(ret, i)
		}
	}
	return append(ret, s)
}

func (q SensorRequestQueue) Len() int { return len(q) }

func (q SensorRequestQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }

func (q SensorRequestQueue) Less(i, j int) bool {
	if q[j].Action == TargetTemperature {
		// i should come before j if j is TargetTemperature
		return true
	} else if q[i].Action == TargetTemperature {
		// j should come before i if i is TargetTemperature
		return false
	} else {
		// otherwise just use alphabetic order
		return strings.Compare(string(q[i].Action), string(q[j].Action)) == -1
	}
}

func (s SensorRequest) String() string {
	switch s.Action {
	case TargetTemperature:
		return fmt.Sprintf("Target temperature %.1f°C", s.Temperature)
	case PowerMode:
		return fmt.Sprintf("Set Power Mode %v", s.Mode)
	case ValveState:
		return fmt.Sprintf("Set Valve State %v", s.ValveState)
	default:
		return fmt.Sprint(s.Action)
	}
}

// Service energenie
type Service struct {
	dev     *ener314.Device
	targets map[string]float64
	queue   map[uint32]SensorRequestQueue
}

func (self *Service) ID() string {
	return "energenie"
}

func round(f float64, dp int) float64 {
	shift := math.Pow(10, float64(dp))
	return math.Floor(f*shift+.5) / shift
}

func emitTemp(msg *ener314.Message, record ener314.Temperature) {
	source := fmt.Sprintf("energenie.%06x", msg.SensorId)
	value := record.Value
	fields := pubsub.Fields{
		"source": source,
		"temp":   round(value, 1),
	}
	ev := pubsub.NewEvent("temp", fields)
	services.Config.AddDeviceToEvent(ev)
	services.Publisher.Emit(ev)
}

func emitVoltage(msg *ener314.Message, record ener314.Voltage) {
	source := fmt.Sprintf("energenie.%06x", msg.SensorId)
	value := record.Value
	fields := pubsub.Fields{
		"source":  source,
		"voltage": round(value, 2),
	}
	ev := pubsub.NewEvent("voltage", fields)
	services.Config.AddDeviceToEvent(ev)
	services.Publisher.Emit(ev)
}

func (self *Service) sendQueuedRequests(sensorId uint32) {
	if qu, ok := self.queue[sensorId]; ok {
		// sort the requests, so TargetTemps are last
		sort.Sort(qu)

		var requeue SensorRequestQueue
		for _, request := range qu {
			log.Printf("%06x Sending %s\n", sensorId, request)
			switch request.Action {
			case TargetTemperature:
				self.dev.TargetTemperature(sensorId, request.Temperature)
			case Identify:
				self.dev.Identify(sensorId)
			case Diagnostics:
				self.dev.Diagnostics(sensorId)
			case Exercise:
				self.dev.ExerciseValve(sensorId)
			case Voltage:
				self.dev.Voltage(sensorId)
			case ValveState:
				self.dev.SetValveState(sensorId, request.ValveState)
			case PowerMode:
				self.dev.SetPowerMode(sensorId, request.Mode)
			}

			if request.Repeat > 0 {
				// requeue any requests to repeat
				request.Repeat -= 1
				requeue = append(requeue, request)
			}
		}

		if len(requeue) > 0 {
			self.queue[sensorId] = requeue
		} else {
			delete(self.queue, sensorId)
		}
	}
}

func (self *Service) handleMessage(msg *ener314.Message) {
	if len(msg.Records) == 0 {
		log.Printf("%06x Announced presence\n", msg.SensorId)
		return
	}

	record := msg.Records[0] // only examine first record
	switch t := record.(type) {
	case ener314.Join:
		log.Printf("%06x Join\n", msg.SensorId)
		self.dev.Join(msg.SensorId)
	case ener314.Temperature:
		log.Printf("%06x Temperature: %.1f°C\n", msg.SensorId, t.Value)
		emitTemp(msg, t)
		// the eTRV is listening - this is the opportunity to send any queued requests
		self.sendQueuedRequests(msg.SensorId)
	case ener314.Voltage:
		log.Printf("%06x Voltage: %.3fV\n", msg.SensorId, t.Value)
		emitVoltage(msg, t)
	case ener314.Diagnostics:
		log.Printf("%06x Diagnostics report: %s\n", msg.SensorId, t)
	}
}

func lookupSensorId(device string) uint32 {
	trv := strings.Replace(device, "thermostat.", "trv.", 1)
	matches := services.Config.LookupDeviceProtocol(trv)
	if sid, ok := matches["energenie"]; ok {
		id, _ := strconv.ParseUint(sid, 16, 32)
		return uint32(id)
	}
	return 0
}

func allSensorIds() []uint32 {
	var ret []uint32
	for sid := range services.Config.Protocols["energenie"] {
		id, _ := strconv.ParseUint(sid, 16, 32)
		ret = append(ret, uint32(id))
	}
	return ret
}

func lookupDevice(sensorId uint32) string {
	sensorSid := fmt.Sprintf("%06x", sensorId)
	device, _ := services.Config.Protocols["energenie"][sensorSid]
	return device
}

func (self *Service) handleThermostat(ev *pubsub.Event) {
	var current float64
	var ok bool
	if current, ok = self.targets[ev.Device()]; !ok {
		current = -1 // target not set
	}

	// use adjusted trv target - this is adjusted down because trv valves barely open +/- 0.1C of the target,
	// which results in running the boiler without heating
	target, ok := ev.Fields["trv"].(float64)
	if !ok {
		log.Println("Error: thermostat event trv field invalid:", ev)
		return
	}
	// resend on the hour
	if current == target && ev.Timestamp.Minute() != 0 {
		return // nothing to do
	}

	// lookup sensorid
	sensorId := lookupSensorId(ev.Device())
	if sensorId != 0 {
		self.queueRequest(sensorId, SensorRequest{Action: TargetTemperature, Temperature: target, Repeat: 2})
	}
	self.targets[ev.Device()] = target
}

func (self *Service) queueRequest(sensorId uint32, request SensorRequest) {
	log.Printf("%06x Queueing %s\n", sensorId, request)
	self.queue[sensorId] = self.queue[sensorId].Append(request)
}

var valveStates = map[string]ener314.ValveState{
	"open":   ener314.VALVE_STATE_OPEN,
	"closed": ener314.VALVE_STATE_CLOSED,
	"auto":   ener314.VALVE_STATE_AUTO,
}

var powerModes = map[string]ener314.PowerMode{
	"normal": ener314.POWER_MODE_NORMAL,
	"low":    ener314.POWER_MODE_LOW,
}

func (self *Service) handleCommand(ev *pubsub.Event) {
	sensorId := lookupSensorId(ev.Device())
	if sensorId == 0 {
		return // command not for us
	}
	switch ev.Command() {
	case "identify":
		self.queueRequest(sensorId, SensorRequest{Action: Identify})
	case "diagnostics":
		self.queueRequest(sensorId, SensorRequest{Action: Diagnostics})
	case "exercise":
		self.queueRequest(sensorId, SensorRequest{Action: Exercise})
	case "voltage":
		self.queueRequest(sensorId, SensorRequest{Action: Voltage})
	case "valvestate":
		if state, ok := valveStates[ev.StringField("state")]; ok {
			self.queueRequest(sensorId, SensorRequest{Action: ValveState, ValveState: state})
		} else {
			log.Println("Valve state: %s not understood", ev.StringField("state"))
		}
	case "powermode":
		if mode, ok := powerModes[ev.StringField("mode")]; ok {
			self.queueRequest(sensorId, SensorRequest{Action: PowerMode, Mode: mode})
		} else {
			log.Println("Power mode: %s not understood", ev.StringField("mode"))
		}
	default:
		log.Println("Command not recognised:", ev.Command())
	}
}

func (self *Service) daily() {
	for _, sensorId := range allSensorIds() {
		// scheduled voltage request
		self.queueRequest(sensorId, SensorRequest{Action: Voltage})
	}
}

func (self *Service) QueryHandlers() services.QueryHandlers {
	return services.QueryHandlers{
		"status":      services.TextHandler(self.queryStatus),
		"identify":    services.TextHandler(self.queryIdentify),
		"diagnostics": services.TextHandler(self.queryDiagnostics),
		"exercise":    services.TextHandler(self.queryExercise),
		"voltage":     services.TextHandler(self.queryVoltage),
		"help": services.StaticHandler("" +
			"status: get status\n" +
			"identify sensor: ask sensor to identify\n" +
			"diagnostics sensor: ask sensor for diagnostics\n" +
			"exercise sensor: ask sensor to exercise\n" +
			"voltage sensor: ask sensor for voltage\n"),
	}
}

func (self *Service) queryStatus(q services.Question) string {
	msg := "Queue:"
	for id, q := range self.queue {
		for _, i := range q {
			msg += fmt.Sprintf("\n%s: %s", lookupDevice(id), i)
		}
	}
	return msg
}

func (self *Service) queryIdentify(q services.Question) string {
	return self.queryX(Identify, q)
}

func (self *Service) queryDiagnostics(q services.Question) string {
	return self.queryX(Diagnostics, q)
}

func (self *Service) queryExercise(q services.Question) string {
	return self.queryX(Exercise, q)
}

func (self *Service) queryVoltage(q services.Question) string {
	return self.queryX(Voltage, q)
}

func (self *Service) queryX(action Action, q services.Question) string {
	if q.Args == "" {
		return "Sensor name required"
	}
	sensorId := lookupSensorId(q.Args)
	if sensorId == 0 {
		return "Sensor not found"
	}
	req := SensorRequest{Action: action}
	self.queueRequest(sensorId, req)
	return fmt.Sprintf("%s queued to sensor: %06x", req, sensorId)
}

func (self *Service) Initialize() {
	self.targets = map[string]float64{}
	self.queue = map[uint32]SensorRequestQueue{}
}

func (self *Service) Run() error {
	self.Initialize()

	ener314.SetLevel(ener314.LOG_INFO)
	dev := ener314.NewDevice()
	self.dev = dev
	err := dev.Start()
	if err != nil {
		return err
	}
	thermostatChannel := services.Subscriber.FilteredChannel("thermostat")
	commandChannel := services.Subscriber.FilteredChannel("command")

	receiveTimer := time.NewTimer(time.Millisecond)
	offset := time.Duration(0)
	every := 24 * time.Hour
	dailyTicker := util.NewScheduler(offset, every)

	for {
		select {
		case <-receiveTimer.C:
			// poll receive
			for msg := dev.Receive(); msg != nil; {
				self.handleMessage(msg)
				// check for any more
				msg = dev.Receive()
			}
			// check again in 100ms
			receiveTimer.Reset(100 * time.Millisecond)
		case ev := <-thermostatChannel:
			self.handleThermostat(ev)
		case ev := <-commandChannel:
			self.handleCommand(ev)
		case <-dailyTicker.C:
			self.daily()
		}
	}
}
