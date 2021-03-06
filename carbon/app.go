package carbon

import (
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/lomik/carbon-clickhouse/helper/RowBinary"
	"github.com/lomik/carbon-clickhouse/receiver"
	"github.com/lomik/carbon-clickhouse/uploader"
	"github.com/lomik/carbon-clickhouse/writer"
	"github.com/lomik/zapwriter"
)

type App struct {
	sync.RWMutex
	Config         *Config
	Writer         *writer.Writer
	Uploader       *uploader.Uploader
	UDP            receiver.Receiver
	TCP            receiver.Receiver
	Pickle         receiver.Receiver
	Collector      *Collector // (!!!) Should be re-created on every change config/modules
	writeChan      chan *RowBinary.WriteBuffer
	exit           chan bool
	ConfigFilename string
}

// New App instance
func New(configFilename string) *App {
	app := &App{
		exit:           make(chan bool),
		ConfigFilename: configFilename,
	}

	return app
}

// configure loads config from config file, schemas.conf, aggregation.conf
func (app *App) configure() error {
	cfg, err := ReadConfig(app.ConfigFilename)
	if err != nil {
		return err
	}

	// carbon-cache prefix
	if hostname, err := os.Hostname(); err == nil {
		hostname = strings.Replace(hostname, ".", "_", -1)
		cfg.Common.MetricPrefix = strings.Replace(cfg.Common.MetricPrefix, "{host}", hostname, -1)
	} else {
		cfg.Common.MetricPrefix = strings.Replace(cfg.Common.MetricPrefix, "{host}", "localhost", -1)
	}

	if cfg.Common.MetricEndpoint == "" {
		cfg.Common.MetricEndpoint = MetricEndpointLocal
	}

	if cfg.Common.MetricEndpoint != MetricEndpointLocal {
		u, err := url.Parse(cfg.Common.MetricEndpoint)

		if err != nil {
			return fmt.Errorf("common.metric-endpoint parse error: %s", err.Error())
		}

		if u.Scheme != "tcp" && u.Scheme != "udp" {
			return fmt.Errorf("common.metric-endpoint supports only tcp and udp protocols. %#v is unsupported", u.Scheme)
		}
	}

	app.Config = cfg

	return nil
}

// ParseConfig loads config from config file
func (app *App) ParseConfig() error {
	app.Lock()
	defer app.Unlock()

	return app.configure()
}

// // ReloadConfig reloads some settings from config
// func (app *App) ReloadConfig() error {
// 	app.Lock()
// 	defer app.Unlock()

// 	var err error
// 	if err = app.configure(); err != nil {
// 		return err
// 	}

// 	// TODO: reload something?

// 	if app.Collector != nil {
// 		app.Collector.Stop()
// 		app.Collector = nil
// 	}

// 	app.Collector = NewCollector(app)

// 	return nil
// }

// Stop all socket listeners
func (app *App) stopListeners() {
	logger := zapwriter.Logger("app")

	if app.TCP != nil {
		app.TCP.Stop()
		app.TCP = nil
		logger.Debug("finished", zap.String("module", "tcp"))
	}

	if app.Pickle != nil {
		app.Pickle.Stop()
		app.Pickle = nil
		logger.Debug("finished", zap.String("module", "pickle"))
	}

	if app.UDP != nil {
		app.UDP.Stop()
		app.UDP = nil
		logger.Debug("finished", zap.String("module", "udp"))
	}
}

func (app *App) stopAll() {
	logger := zapwriter.Logger("app")

	app.stopListeners()

	if app.Collector != nil {
		app.Collector.Stop()
		app.Collector = nil
		logger.Debug("finished", zap.String("module", "collector"))
	}

	if app.Writer != nil {
		app.Writer.Stop()
		app.Writer = nil
		logger.Debug("finished", zap.String("module", "writer"))
	}

	if app.Uploader != nil {
		app.Uploader.Stop()
		app.Uploader = nil
		logger.Debug("finished", zap.String("module", "uploader"))
	}

	if app.exit != nil {
		close(app.exit)
		app.exit = nil
		logger.Debug("close(app.exit)", zap.String("module", "app"))
	}
}

// Stop force stop all components
func (app *App) Stop() {
	app.Lock()
	defer app.Unlock()
	app.stopAll()
}

// Start starts
func (app *App) Start() (err error) {
	app.Lock()
	defer app.Unlock()

	defer func() {
		if err != nil {
			app.stopAll()
		}
	}()

	conf := app.Config

	runtime.GOMAXPROCS(conf.Common.MaxCPU)

	app.writeChan = make(chan *RowBinary.WriteBuffer)

	/* WRITER start */
	app.Writer = writer.New(
		app.writeChan,
		conf.Data.Path,
		conf.Data.FileInterval.Value(),
	)
	app.Writer.Start()
	/* WRITER end */

	/* UPLOADER start */
	dataTables := conf.ClickHouse.DataTables
	if dataTables == nil {
		dataTables = make([]string, 0)
	}

	if conf.ClickHouse.DataTable != "" {
		exists := false
		for i := 0; i < len(dataTables); i++ {
			if dataTables[i] == conf.ClickHouse.DataTable {
				exists = true
			}
		}

		if !exists {
			dataTables = append(dataTables, conf.ClickHouse.DataTable)
		}
	}

	reverseDataTables := conf.ClickHouse.ReverseDataTables
	if reverseDataTables == nil {
		reverseDataTables = make([]string, 0)
	}

	app.Uploader = uploader.New(
		uploader.Path(conf.Data.Path),
		uploader.ClickHouse(conf.ClickHouse.Url),
		uploader.DataTables(dataTables),
		uploader.ReverseDataTables(reverseDataTables),
		uploader.DataTimeout(conf.ClickHouse.DataTimeout.Value()),
		uploader.TreeTable(conf.ClickHouse.TreeTable),
		uploader.ReverseTreeTable(conf.ClickHouse.ReverseTreeTable),
		uploader.TreeDate(conf.ClickHouse.TreeDate),
		uploader.TreeTimeout(conf.ClickHouse.TreeTimeout.Value()),
		uploader.InProgressCallback(app.Writer.IsInProgress),
		uploader.Threads(app.Config.ClickHouse.Threads),
	)
	app.Uploader.Start()
	/* UPLOADER end */

	/* RECEIVER start */
	if conf.Tcp.Enabled {
		app.TCP, err = receiver.New(
			"tcp://"+conf.Tcp.Listen,
			receiver.ParseThreads(runtime.GOMAXPROCS(-1)*2),
			receiver.WriteChan(app.writeChan),
		)

		if err != nil {
			return
		}
	}

	if conf.Udp.Enabled {
		app.UDP, err = receiver.New(
			"udp://"+conf.Udp.Listen,
			receiver.ParseThreads(runtime.GOMAXPROCS(-1)*2),
			receiver.WriteChan(app.writeChan),
		)

		if err != nil {
			return
		}
	}

	if conf.Pickle.Enabled {
		app.Pickle, err = receiver.New(
			"pickle://"+conf.Pickle.Listen,
			receiver.ParseThreads(runtime.GOMAXPROCS(-1)*2),
			receiver.WriteChan(app.writeChan),
		)

		if err != nil {
			return
		}
	}
	/* RECEIVER end */

	/* COLLECTOR start */
	app.Collector = NewCollector(app)
	/* COLLECTOR end */

	return
}

// ClearTreeExistsCache in Uploader
func (app *App) ClearTreeExistsCache() {
	app.Lock()
	up := app.Uploader
	app.Unlock()

	if up != nil {
		go up.ClearTreeExistsCache()
	}
}

// Loop ...
func (app *App) Loop() {
	app.RLock()
	exitChan := app.exit
	app.RUnlock()

	if exitChan != nil {
		<-app.exit
	}
}
