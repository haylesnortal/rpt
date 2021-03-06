package rpt

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

type RptClient struct {
	DBPrimary   DBClient
	DBSecondary DBClient
	Operations  chan *DBOperationSet
	API         APIServer
	Logger      *Logger

	currentLog *Log
	loglvl     string
	keepAlive  bool
	state      chan *InternalStateChange
}

func NewRpt(primary, secondary DBClient, loglvl string) (*RptClient, error) {
	c := make(chan *DBOperationSet, 50)
	s := make(chan *InternalStateChange, 3)
	return &RptClient{
		DBPrimary:   primary,
		DBSecondary: secondary,
		Operations:  c,
		state:       s,
		keepAlive:   false,
		loglvl:      loglvl,
	}, nil
}

// func NewRptFromConfig(configPath string) (*RptClient, error) {
// 	return NewRpt()
// }

func NewRptFromEnvironment() (*RptClient, error) {

	// Gather env vars

	primaryHost := strings.SplitN(os.Getenv("RPT_PRIMARY_HOST"), ":", 2)[1]     //required
	primaryHostType := strings.SplitN(os.Getenv("RPT_PRIMARY_HOST"), ":", 2)[0] //required
	primaryPort := os.Getenv("RPT_PRIMARY_PORT")                                //defaults to 5432
	primaryUser := os.Getenv("RPT_PRIMARY_USER")                                //required
	primaryPass := os.Getenv("RPT_PRIMARY_PASS")                                //required
	primarySSLMode := os.Getenv("RPT_PRIMARY_SSLMODE")                          //defaults to disable

	secondaryHost := strings.SplitN(os.Getenv("RPT_SECONDARY_HOST"), ":", 2)[1]     //required
	secondaryHostType := strings.SplitN(os.Getenv("RPT_SECONDARY_HOST"), ":", 2)[0] //required
	secondaryPort := os.Getenv("RPT_SECONDARY_PORT")                                //defaults to 5432
	secondaryUser := os.Getenv("RPT_SECONDARY_USER")                                //required
	secondaryPass := os.Getenv("RPT_SECONDARY_PASS")                                //required
	secondarySSLMode := os.Getenv("RPT_SECONDARY_SSLMODE")                          //defaults to disable

	seedFile := os.Getenv("RPT_SEED_FILE") //optional

	useAPI := os.Getenv("RPT_API")                       //defaults to false
	apiBasePath := os.Getenv("RPT_API_BASEPATH")         //defaults to api
	apiListenAddress := os.Getenv("RPT_API_LISTEN_ADDR") //defaults to localhost:5000

	rptLogLvl := os.Getenv("RPT_LOG_LVL") //defaults to INFO

	log.Println(primaryHost)
	log.Println(primaryHostType)

	// populate defaults

	l := &Logger{}

	if primaryPort == "" {
		primaryPort = "5432"
	}

	if secondaryPort == "" {
		secondaryPort = "5432"
	}

	if rptLogLvl == "" {
		rptLogLvl = "INFO"
	}

	// validate

	primaryPortInt, err := strconv.Atoi(primaryPort)
	if err != nil {
		return nil, err
	}

	primarySSLMode, err = validateSSL(primarySSLMode)
	if err != nil {
		return nil, err
	}

	secondaryPortInt, err := strconv.Atoi(secondaryPort)
	if err != nil {
		return nil, err
	}

	secondarySSLMode, err = validateSSL(secondarySSLMode)
	if err != nil {
		return nil, err
	}

	// create objects

	db1 := getDBClient(primaryHostType, primaryHost, primaryUser, primaryPass, primarySSLMode, primaryPortInt, l)
	db2 := getDBClient(secondaryHostType, secondaryHost, secondaryUser, secondaryPass, secondarySSLMode, secondaryPortInt, l)

	err = db1.Connect()
	if err != nil {
		return nil, err
	}
	err = db2.Connect()
	if err != nil {
		return nil, err
	}

	dbo := newDBOperationSet(nil)
	r, err := NewRpt(db1, db2, rptLogLvl)
	r.Logger = l
	r.newLog()

	if seedFile != "" {
		ds, errs := ImportDBDataSet(seedFile)
		if len(errs) > 0 {
			return nil, errs[0]
		}

		seed := SeedData(db1, ds)
		dbo.Operations = append(dbo.Operations, seed)
		r.Operations <- dbo
	}

	r.API = *validateAPI(useAPI, apiBasePath, apiListenAddress)

	return r, nil
}

func validateSSL(s string) (string, error) {

	switch s {
	case "disable":
		log.Println("SSL Mode: disable")
	case "require":
		log.Println("SSL Mode: require")
	case "verify-ca":
		log.Println("SSL Mode: verify-ca")
	case "verify-full":
		log.Println("SSL Mode: verify-full")
	case "":
		log.Println("SSL Mode: not set. Defaulting to disable")
		s = "disable"
	default:
		return "", fmt.Errorf("rpt: invalid SSL mode")
	}

	return s, nil
}

func validateAPI(api, base, addr string) *APIServer {

	a := &APIServer{}
	if api != "TRUE" {
		return nil
	}

	if base == "" {
		base = "/api"
	}

	if addr == "" {
		addr = ":5000"
	}

	a.BasePath = base
	a.ListenAddr = addr

	return a
}

func (r *RptClient) Init() {

	r.currentLog.Debugf("RPT client initialized")

	r.keepAlive = false
	r.currentLog.Debugf("keepAlive set to false")

	if r.API.ListenAddr != "" {
		r.currentLog.Debugf("Initializing API")
		go r.API.Init(r.Operations, r.state, r.DBPrimary, r.DBSecondary, r.Logger, r.loglvl)
		r.keepAlive = true
		r.currentLog.Debugf("keepAlive set to true")
	}

	go r.ListenForStateChange()
	go r.Process()

	r.currentLog.Debugf("Creating console output")
	oot := NewConsoleOutput()
	oot.Connect()
	r.Logger.AddLogOutput(oot)

	r.currentLog.Debugf("Entering keepAlive loop")
	r.state <- newInternalState("cycle_log")
	for r.keepAlive == true {
		// Hold your horses.
	}
	r.currentLog.Debugf("Leaving keepAlive loop and exiting application")
	r.state <- newInternalState("cycle_log")
}

func (r *RptClient) Process() {
	r.currentLog.Debugf("Initializing RPT client operation processing")
	for opSet := range r.Operations {

		for _, op := range opSet.Operations {

			op.Start()

		}

		log.Println(string(ToJSON(opSet)))
	}
	r.currentLog.Debugf("RPT client operation processing exited")
}

func (r *RptClient) ListenForStateChange() {
	r.currentLog.Debugf("Initializing internal state change listener")
	waitingForShutdown := false

	for sc := range r.state {

		if sc.NewState == "stop" {
			log.Println("Received STOP")
			break
		}

		if sc.NewState == "process_then_stop" {
			log.Println("Received PROCESS THEN STOP")
			waitingForShutdown = true
		}

		if sc.NewState == "processing_complete" && waitingForShutdown {
			log.Println("Received PROCESSING COMPLETE - SHUTTING DOWN")
			break
		}

		if sc.NewState == "processing_complete" && !waitingForShutdown {
			log.Println("Received PROCESSING COMPLETE")
		}

		if sc.NewState == "cycle_log" {
			r.currentLog.Debugf("State change cycle_log received")
			r.newLog()
		}

	}

	r.currentLog.Debugf("Exiting state change listener and setting keepAlive to false")
	r.keepAlive = false
}

func (r *RptClient) newLog() {
	if r.currentLog != nil {
		r.currentLog.Debugf("Writing log to outputs")
		r.Logger.WriteLog(r.currentLog)
	}
	r.currentLog = NewLog(r.loglvl, "api_log")
	r.currentLog.Debugf("New log created")
}

func getDBClient(clientType, host, user, password, ssl string, port int, logger *Logger) DBClient {
	if clientType == "postgres" {
		return NewPostgresClient(host, user, password, ssl, port, logger)
	}

	return nil
}
