package app

import (
	"encoding/json"
	"fmt"
	"msfs2020-gopilot/internal/config"
	"msfs2020-gopilot/internal/util"
	"msfs2020-gopilot/internal/webserver"
	"msfs2020-gopilot/internal/websockets"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/buger/jsonparser"
	alphafoxtrot "github.com/grumpypixel/go-airport-finder"
	"github.com/grumpypixel/msfs2020-simconnect-go/simconnect"
	log "github.com/sirupsen/logrus"
)

type Message struct {
	Type  string                 `json:"type"`
	Meta  string                 `json:"meta"`
	Data  map[string]interface{} `json:"data"`
	Debug string                 `json:"debug"`
}

const (
	appTitle                   = "MSFS2020-GoPilot"
	dataDir                    = "data/"
	airportsDataDir            = dataDir + "/ourairports"
	contentTypeHTML            = "text/html"
	contentTypeText            = "text/plain; charset=utf-8"
	defaultAirportSearchRadius = 50 * 1000.0
	defaultMaxAirportCount     = 10
	connectRetryInterval       = 1 // seconds
	receiveDataInterval        = 1 // milliseconds
	shutdownDelay              = 3 // seconds
	broadcastInterval          = 250
	// dataRequestInterval        = 200 // milliseconds
)

type App struct {
	cfg              *config.Config
	requestManager   *RequestManager
	socket           *websockets.WebSocket
	mate             *simconnect.SimMate
	airportFinder    *alphafoxtrot.AirportFinder
	done             chan interface{}
	flightSimVersion string
	eventListener    *simconnect.EventListener
}

func NewApp(cfg *config.Config) *App {
	return &App{
		cfg:            cfg,
		requestManager: NewRequestManager(),
		done:           make(chan interface{}, 1),
		airportFinder:  alphafoxtrot.NewAirportFinder(),
	}
}

func (app *App) Run() error {
	app.addEventListeners()

	app.socket = websockets.NewWebSocket()
	go app.handleSocketMessages()

	serverShutdown := make(chan bool, 1)
	defer close(serverShutdown)

	app.listNetworkInterfaces()

	log.Info("Loading airport database...")
	airportFinderOptions := alphafoxtrot.PresetLoadOptions(airportsDataDir)
	airportFinderFilter := alphafoxtrot.AirportTypeAll
	if errs := app.airportFinder.Load(airportFinderOptions, airportFinderFilter); len(errs) > 0 {
		log.Warn("Airport finder will not be available, because of the following errors:")
		for err := range errs {
			log.Error(err)
		}
		app.airportFinder = nil
	}

	log.Info("Loading ", simconnect.SimConnectDLL, "...")
	if err := simconnect.Initialize(app.cfg.SimConnectDLLPath); err != nil {
		return err
	}

	app.mate = simconnect.NewSimMate()
	app.initWebServer(app.cfg.ServerAddress, serverShutdown)

	stopBroadcast := make(chan interface{}, 1)
	defer close(stopBroadcast)
	go app.Broadcast(broadcastInterval*time.Millisecond, stopBroadcast)

	retryInterval := connectRetryInterval * time.Second
	timeout := time.Second * time.Duration(app.cfg.ConnectionTimeout)
	if err := app.connect(app.cfg.ConnectionName, retryInterval, timeout); err != nil {
		return err
	}

	go app.handleTerminationSignal()

	stopEventHandler := make(chan interface{}, 1)
	defer close(stopEventHandler)

	requestInterval := time.Duration(app.cfg.DataRequestInterval) * time.Millisecond
	receiveInterval := receiveDataInterval * time.Millisecond
	go app.mate.HandleEvents(requestInterval, receiveInterval, stopEventHandler, app.eventListener)

	<-app.done
	defer close(app.done)

	log.Info("Shutting down...")

	stopEventHandler <- true
	stopBroadcast <- true
	serverShutdown <- true

	if err := app.disconnect(); err != nil {
		return err
	}

	log.Info("Taking a quick nap...")
	time.Sleep(shutdownDelay * time.Second)
	return nil
}

func (app *App) addEventListeners() {
	app.eventListener = &simconnect.EventListener{
		OnOpen:      app.OnOpen,
		OnQuit:      app.OnQuit,
		OnDataReady: app.OnDataReady,
		OnEventID:   app.OnEventID,
		OnException: app.OnException,
	}
}

func (app *App) initWebServer(address string, shutdown chan bool) {
	htmlHeaders := app.Headers(contentTypeHTML)
	textHeaders := app.Headers(contentTypeText)
	webServer := webserver.NewWebServer(address, shutdown)
	htmlDir := "assets/html"
	routes := []webserver.Route{
		{Pattern: "/", Handler: app.staticContentHandler(htmlHeaders, "/", filepath.Join(htmlDir, "vfrmap.html"))},
		{Pattern: "/vfrmap", Handler: app.staticContentHandler(htmlHeaders, "/vfrmap", filepath.Join(htmlDir, "vfrmap.html"))},
		{Pattern: "/mehmap", Handler: app.staticContentHandler(htmlHeaders, "/mehmap", filepath.Join(htmlDir, "mehmap.html"))},
		{Pattern: "/setdata", Handler: app.staticContentHandler(htmlHeaders, "/setdata", filepath.Join(htmlDir, "setdata.html"))},
		{Pattern: "/airports", Handler: app.staticContentHandler(htmlHeaders, "/airports", filepath.Join(htmlDir, "airports.html"))},
		{Pattern: "/teleport", Handler: app.staticContentHandler(htmlHeaders, "/teleport", filepath.Join(htmlDir, "teleporter.html"))},
		{Pattern: "/steepturns", Handler: app.staticContentHandler(htmlHeaders, "/steepturns", filepath.Join(htmlDir, "steepturns.html"))},
		// {Pattern: "/experimental", Handler: app.staticContentHandler(htmlHeaders, "/experimental", filepath.Join(htmlDir, "experimental/index.html"))},
		{Pattern: "/debug", Handler: app.generatedContentHandler(textHeaders, "/debug", app.DebugGenerator)},
		{Pattern: "/simvars", Handler: app.generatedContentHandler(textHeaders, "/simvars", app.simvarsGenerator)},
		{Pattern: "/ws", Handler: app.socket.Serve},
	}

	log.Info("Starting web server...")
	staticAssetsDir := "/assets/"
	webServer.Run(routes, staticAssetsDir)

	log.Info("Web Server listening on ", address)
}

// https://golang-examples.tumblr.com/post/99458329439/get-local-ip-addresses
func (app *App) listNetworkInterfaces() {
	list, err := net.Interfaces()
	if err != nil {
		return
	}
	networkInterfaces := []string{}
	for _, iface := range list {
		str := fmt.Sprintf("%s: ", iface.Name)
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for j, addr := range addrs {
			str += fmt.Sprintf("%v", addr)
			if j < len(addrs) {
				str += ", "
			}
		}
		networkInterfaces = append(networkInterfaces, str)
	}
	str := ""
	for i, iface := range networkInterfaces {
		str += fmt.Sprintf(" %d %s\n", i, iface)
	}
	log.Info("Your network interfaces:\n", str)
}

func (app *App) connect(name string, retryInterval, timeout time.Duration) error {
	log.Info("Trying to establish a connection with the Simulator...")
	connectTicker := time.NewTicker(retryInterval)
	defer connectTicker.Stop()

	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()

	count := 0
	for {
		select {
		case <-connectTicker.C:
			count++
			if err := app.mate.Open(name); err != nil {
				if count%10 == 0 {
					log.Info("Connection attempts...", count)
				}
			} else {
				return nil
			}
		case <-timeoutTimer.C:
			return fmt.Errorf("establishing a connection with the simulator timed out painfully")
		default:
		}
	}
}

func (app *App) disconnect() error {
	log.Info("Closing connection")
	if err := app.mate.Close(); err != nil {
		return err
	}
	return nil
}

func (app *App) handleTerminationSignal() {
	sigterm := make(chan os.Signal, 1)
	defer close(sigterm)
	signal.Notify(sigterm, os.Interrupt, syscall.SIGTERM)

	for {
		select {
		case <-sigterm:
			log.Info("SIGTERM. Terminating process...")
			app.done <- true
			return
		}
	}
}

func (app *App) handleSocketMessages() {
	for {
		select {
		case event := <-app.socket.EventReceiver:
			eventType := event.Type
			connID := event.Connection.UUID()
			switch eventType {
			case websockets.SocketEventConnected:
				log.Info("Client connected: ", connID)

			case websockets.SocketEventDisconnected:
				log.Info("Client disconnected: ", connID)
				app.removeRequests(connID)

			case websockets.SocketEventMessage:
				msg := &Message{}
				json.Unmarshal(event.Data, msg)
				log.Debug("Message", connID, msg)

				switch msg.Type {
				case "airports":
					app.handleAirportsMessage(msg, connID)

				case "deregister":
					app.handleDeregisterMessage(msg, connID)

				case "echo":
					app.handleEchoMessage(msg, connID)

				case "ping":
					app.handlePingMessage(msg, connID)

				case "register":
					app.handleRegisterMessage(msg, event.Data, connID)

				case "setdata":
					app.handleSetDataMessage(msg)

				case "teleport":
					app.handleTeleportMessage(msg)

				default:
					log.Warn("Received unknown message with type: %s\n data: %v\n sender: %s\n", msg.Type, msg.Data, connID)
				}
			}
		default:
		}
	}
}

func (app *App) handleAirportsMessage(msg *Message, connID string) {
	latitude, ok := util.FloatFromJson("latitude", msg.Data)
	if !ok {
		return
	}
	longitude, ok := util.FloatFromJson("longitude", msg.Data)
	if !ok {
		return
	}
	radiusInMeters, ok := util.FloatFromJson("radius", msg.Data)
	if !ok {
		radiusInMeters = defaultAirportSearchRadius
	}
	maxAirports, ok := util.IntFromJson("maxAirports", msg.Data)
	if !ok {
		maxAirports = defaultMaxAirportCount
	}

	airportFilter := alphafoxtrot.AirportTypeAll
	filter, ok := util.StringFromJson("filter", msg.Data)
	if ok {
		airportFilter = 0
		filters := strings.Split(filter, "|")
		for _, str := range filters {
			f := alphafoxtrot.AirportTypeFromString(str)
			airportFilter |= f
		}
	}

	go func() {
		if app.airportFinder == nil {
			log.Warn("Airports database not available")
			return
		}

		log.Info("Finding airports...")
		airports := app.airportFinder.FindNearestAirports(latitude, longitude, radiusInMeters, maxAirports, uint64(airportFilter))
		if len(airports) == 0 {
			log.Info("No airports found around ", latitude, ", ", longitude)
		}

		airportList := make([]map[string]interface{}, 0)
		for _, airport := range airports {
			ap := make(map[string]interface{})
			ap["type"] = airport.Type
			ap["icao"] = airport.ICAOCode
			ap["name"] = airport.Name
			ap["latitude"] = util.FloatToString(airport.LatitudeDeg)
			ap["longitude"] = util.FloatToString(airport.LongitudeDeg)
			ap["elevation"] = fmt.Sprint(airport.ElevationFt)
			airportList = append(airportList, ap)
		}

		reply := map[string]interface{}{
			"type": "airports",
			"meta": msg.Meta,
			"data": airportList,
		}

		log.Infof("Found %d airports", len(airports))
		if buf, err := json.Marshal(reply); err == nil {
			app.socket.Send(connID, buf)
			log.Debug(airportList)
		} else {
			log.Error(err)
		}
	}()
}

func (app *App) handleDeregisterMessage(msg *Message, connID string) {
	app.removeRequests(connID)
}

func (app *App) handleEchoMessage(msg *Message, connID string) {
	if buf, err := json.Marshal(msg); err == nil {
		app.socket.Send(connID, buf)
	}
}

func (app *App) handlePingMessage(msg *Message, connID string) {
	reply := map[string]interface{}{
		"type": "pong",
		"meta": msg.Meta,
		"data": time.Now().String,
	}
	if buf, err := json.Marshal(reply); err == nil {
		app.socket.Send(connID, buf)
	}
}

func (app *App) handleRegisterMessage(msg *Message, raw []byte, connID string) {
	request := NewRequest(connID, msg.Meta)
	jsonparser.ArrayEach(raw, func(value []byte, dataType jsonparser.ValueType, offset int, err error) {
		n, _, _, _ := jsonparser.Get(value, "name")
		u, _, _, _ := jsonparser.Get(value, "unit")
		t, _, _, _ := jsonparser.Get(value, "type")
		m, _, _, _ := jsonparser.Get(value, "moniker")
		name := string(n)
		unit := string(u)
		typ := simconnect.StringToDataType(string(t))
		moniker := string(m)
		defineID := app.mate.AddSimVar(name, unit, typ)
		log.Info(fmt.Sprintf("Added SimVar with id: %d, name: %s, unit: %s, type: %d", defineID, name, unit, typ))
		request.Add(defineID, name, moniker)
	}, "data")
	app.requestManager.AddRequest(request)
	log.Info("Added request ", request)
}

func (app *App) handleSetDataMessage(msg *Message) {
	if !app.mate.IsConnected() {
		log.Warn("Not connected to SimConnect. Ignoring SetDataMessage.")
		return
	}
	name, ok := util.StringFromJson("name", msg.Data)
	if !ok {
		return
	}
	unit, ok := util.StringFromJson("unit", msg.Data)
	if !ok {
		return
	}
	value, ok := util.FloatFromJson("value", msg.Data)
	if !ok {
		return
	}
	if err := app.mate.SetSimObjectData(name, unit, value, simconnect.DataTypeFloat64); err != nil {
		log.Error(err)
	}
}

func (app *App) handleTeleportMessage(msg *Message) {
	if !app.mate.IsConnected() {
		log.Warn("Not connected to SimConnect. Ignoring TeleportMessage.")
		return
	}
	latitude, ok := util.FloatFromJson("latitude", msg.Data)
	if !ok {
		return
	}
	longitude, ok := util.FloatFromJson("longitude", msg.Data)
	if !ok {
		return
	}
	altitude, ok := util.FloatFromJson("altitude", msg.Data)
	if !ok {
		return
	}
	heading, ok := util.FloatFromJson("heading", msg.Data)
	if !ok {
		return
	}
	airspeed, ok := util.FloatFromJson("airspeed", msg.Data)
	if !ok {
		return
	}

	bank := 0.0
	pitch := 0.0

	app.mate.SetSimObjectData("PLANE LATITUDE", "degrees", latitude, simconnect.DataTypeFloat64)
	app.mate.SetSimObjectData("PLANE LONGITUDE", "degrees", longitude, simconnect.DataTypeFloat64)
	app.mate.SetSimObjectData("PLANE ALTITUDE", "feet", altitude, simconnect.DataTypeFloat64)
	app.mate.SetSimObjectData("PLANE HEADING DEGREES TRUE", "degrees", heading, simconnect.DataTypeFloat64)
	app.mate.SetSimObjectData("AIRSPEED TRUE", "knot", airspeed, simconnect.DataTypeFloat64)
	app.mate.SetSimObjectData("PLANE BANK DEGREES", "degrees", bank, simconnect.DataTypeFloat64)
	app.mate.SetSimObjectData("PLANE PITCH DEGREES", "degrees", pitch, simconnect.DataTypeFloat64)

	log.Info("Teleporting to lat: %f lng: %f alt: %f hdg: %f spd: %f bnk: %f pit: %f",
		latitude, longitude, altitude, heading, airspeed, bank, pitch)
}

func (app *App) removeRequests(connID string) {
	temp := make([]*Request, 0)
	removed := make([]*Request, 0)
	for _, request := range app.requestManager.Requests {
		if request.ClientID != connID {
			temp = append(temp, request)
		} else {
			removed = append(removed, request)
		}
	}
	app.requestManager.Requests = temp
	for _, request := range removed {
		for defineID, v := range request.Vars {
			count := app.requestManager.RefCount(v.Name)
			if count == 0 {
				app.mate.RemoveSimVar(defineID)
				log.Debug("Removed SimVar", defineID)
			}
		}
	}
}

func (app *App) OnOpen(applName, applVersion, applBuild, simConnectVersion, simConnectBuild string) {
	log.Info("Connected \\o/")
	app.flightSimVersion = fmt.Sprintf(
		"Flight Simulator says:\n Name: %s\n Version: %s (build %s)\n SimConnect: %s (build %s)",
		applName, applVersion, applBuild, simConnectVersion, simConnectBuild)
	log.Info(app.flightSimVersion)
	log.Info("CLEAR PROP!")
}

func (app *App) OnQuit() {
	log.Info("Disconnected (︶︹︶)")
	app.done <- true
}

func (app *App) OnEventID(eventID simconnect.DWord) {
	log.Info("Received event ID: ", eventID)
}

func (app *App) OnException(exceptionCode simconnect.DWord) {
	log.Error("Exception: ", exceptionCode)
}

// func (app *App) OnSimObjectData(data *simconnect.RecvSimObjectData) {
// 	// pass
// }

// func (app *App) OnSimObjectDataByType(data *simconnect.RecvSimObjectDataByType) {
// 	// pass
// }

func (app *App) OnDataReady() {
	for _, request := range app.requestManager.Requests {
		msg := map[string]interface{}{
			"type": "simvars",
			"meta": request.Meta,
		}

		vars := make(map[string]interface{})
		for defineID, v := range request.Vars {
			value, dataType, ok := app.mate.SimVarValueAndDataType(defineID)
			if !ok || value == nil {
				continue
			}
			switch dataType {
			case simconnect.DataTypeInt32:
				vars[v.Moniker] = simconnect.ValueToInt32(value)

			case simconnect.DataTypeInt64:
				vars[v.Moniker] = simconnect.ValueToInt64(value)

			case simconnect.DataTypeFloat32:
				vars[v.Moniker] = simconnect.ValueToFloat32(value)

			case simconnect.DataTypeFloat64:
				vars[v.Moniker] = simconnect.ValueToFloat64(value)

			case simconnect.DataTypeString8,
				simconnect.DataTypeString32,
				simconnect.DataTypeString64,
				simconnect.DataTypeString128,
				simconnect.DataTypeString256,
				simconnect.DataTypeString260,
				simconnect.DataTypeStringV:
				vars[v.Moniker] = simconnect.ValueToString(value)
			}
		}
		msg["data"] = vars
		recipient := request.ClientID
		if buf, err := json.Marshal(msg); err == nil {
			app.socket.Send(recipient, buf)
		}
	}
}

func (app *App) Broadcast(broadcastInterval time.Duration, stop chan interface{}) {
	broadcastTicker := time.NewTicker(broadcastInterval)
	defer broadcastTicker.Stop()

	for {
		select {
		case <-broadcastTicker.C:
			if err := app.BroadcastStatusMessage(); err != nil {
				log.Error(err)
			}
		case <-stop:
			log.Info("Stopped broadcasting")
			return
		}
	}
}

func (app *App) BroadcastStatusMessage() error {
	data := map[string]interface{}{"simconnect": app.mate.IsConnected()}
	msg := map[string]interface{}{"type": "status", "data": data}
	buf, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	app.socket.Broadcast(buf)
	return nil
}

func (app *App) Headers(contentType string) map[string]string {
	headers := map[string]string{
		"Access-Control-Allow-Origin": "*",
		"Cache-Control":               "no-cache, no-store, must-revalidate",
		"Pragma":                      "no-cache",
		"Expires":                     "0",
		"Content-Type":                contentType,
	}
	return headers
}

func (app *App) staticContentHandler(headers map[string]string, urlPath, filePath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != urlPath {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		for key, value := range headers {
			w.Header().Set(key, value)
		}
		http.ServeFile(w, r, filePath)
	}
}

func (app *App) generatedContentHandler(headers map[string]string, urlPath string, generator func(w http.ResponseWriter)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != urlPath {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		for key, value := range headers {
			w.Header().Set(key, value)
		}
		w.WriteHeader(http.StatusOK)
		generator(w)
	}
}

func (app *App) simvarsGenerator(w http.ResponseWriter) {
	fmt.Fprintf(w, "%s\n\n", appTitle)
	fmt.Fprintf(w, "%s\n", app.simVars())
}

func (app *App) DebugGenerator(w http.ResponseWriter) {
	fmt.Fprintf(w, "%s\n\n", appTitle)
	if len(app.flightSimVersion) > 0 {
		fmt.Fprintf(w, "%s\n\n", app.flightSimVersion)
	}
	fmt.Fprintf(w, "SimConnect\n  initialized: %v\n  conncected: %v\n\n", simconnect.IsInitialized(), app.mate.IsConnected())
	fmt.Fprintf(w, "Clients: %d\n", app.socket.ConnectionCount())
	uuids := app.socket.ConnectionUUIDs()
	for i, uuid := range uuids {
		fmt.Fprintf(w, "  %02d: %s\n", i, uuid)
	}
	fmt.Fprintf(w, "\n")
	fmt.Fprintf(w, "%s\n\n", app.simVars())
	fmt.Fprintf(w, "%s\n", app.requests())
}

func (app *App) requests() string {
	var dump string
	dump += fmt.Sprintf("Requests: %d\n", app.requestManager.RequestCount())
	for i, request := range app.requestManager.Requests {
		dump += fmt.Sprintf("  %02d: Client: %s Vars: %d Meta: %s\n", i+1, request.ClientID, len(request.Vars), request.Meta)
		for j, simVar := range request.Vars {
			dump += fmt.Sprintf("    %02d: name: %s moniker: %s\n", j, simVar.Name, simVar.Moniker)
		}
	}
	return dump
}

func (app *App) simVars() string {
	indent := "  "
	dump := app.mate.SimVarDump(indent)
	str := strings.Join(dump[:], "\n")
	return fmt.Sprintf("SimVars: %d\n", len(dump)) + str
}
