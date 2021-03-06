// Copyright 2020-2022 The OS-NVR Authors.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation; either version 2 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package nvr

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"net/http"
	"nvr/pkg/group"
	"nvr/pkg/log"
	"nvr/pkg/monitor"
	"nvr/pkg/storage"
	"nvr/pkg/system"
	"nvr/pkg/video"
	"nvr/pkg/web"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Run .
func Run() error {
	envFlag := flag.String("env", "", "path to env.yaml")
	flag.Parse()

	if *envFlag == "" {
		flag.Usage()
		return nil
	}

	envPath, err := filepath.Abs(*envFlag)
	if err != nil {
		return fmt.Errorf("could not get absolute path of env.yaml: %w", err)
	}

	wg := &sync.WaitGroup{}
	app, err := newApp(envPath, wg, hooks)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := hooks.appRun(ctx); err != nil {
		cancel()
		return err
	}

	fatal := make(chan error, 1)
	go func() { fatal <- app.run(ctx) }()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err = <-fatal:
		app.log.Info().Src("app").Msgf("fatal error: %v", err)
	case signal := <-stop:
		app.log.Info().Msg("") // New line.
		app.log.Info().Src("app").Msgf("received %v, stopping", signal)
	}

	app.monitorManager.StopAll()
	app.log.Info().Src("app").Msg("Monitors stopped.")

	cancel()
	wg.Wait()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()

	if err != nil {
		return err
	}
	return app.server.Shutdown(ctx2)
}

func newApp(envPath string, wg *sync.WaitGroup, hooks *hookList) (*App, error) { //nolint:funlen
	// Environment config.
	envYAML, err := os.ReadFile(envPath)
	if err != nil {
		return nil, fmt.Errorf("could not read env.yaml: %w", err)
	}

	env, err := storage.NewConfigEnv(envPath, envYAML)
	if err != nil {
		return nil, fmt.Errorf("could not get environment config: %w", err)
	}
	hooks.env(*env)

	// Logs.
	logDBpath := filepath.Join(env.StorageDir, "logs.db")
	logger := log.NewLogger(wg, hooks.logSource)
	hooks.log(logger)

	logDB := log.NewDB(logDBpath, wg)

	general, err := storage.NewConfigGeneral(env.ConfigDir)
	if err != nil {
		return nil, fmt.Errorf("could not get general config: %w", err)
	}

	// Video server.
	videoServer := video.NewServer(logger, wg, env.RTSPport, env.HLSport)

	// Monitors.
	monitorConfigDir := filepath.Join(env.ConfigDir, "monitors")
	monitorManager, err := monitor.NewManager(
		monitorConfigDir,
		*env,
		logger,
		videoServer,
		hooks.monitor(),
	)
	if err != nil {
		return nil, fmt.Errorf("could not create monitor manager: %w", err)
	}

	// Monitor groups.
	groupConfigDir := filepath.Join(env.ConfigDir, "groups")
	groupManager, err := group.NewManager(groupConfigDir)
	if err != nil {
		return nil, fmt.Errorf("could not create monitor manager: %w", err)
	}

	// Authentication.
	if hooks.newAuthenticator == nil {
		return nil, fmt.Errorf( //nolint:goerr113,stylecheck
			"No authentication addon enabled. Please enable one in 'config/env.yaml'")
	}

	a, err := hooks.newAuthenticator(*env, logger)
	if err != nil {
		return nil, fmt.Errorf("could not create authenticator: %w", err)
	}
	hooks.auth(a)

	// Storage.
	storageManager := storage.NewManager(env.StorageDir, general, logger)
	hooks.storage(storageManager)
	crawler := storage.NewCrawler(storageManager.RecordingsDir())

	// Time zone.
	timeZone, err := system.TimeZone()
	if err != nil {
		return nil, err
	}

	// Templates.
	t, err := web.NewTemplater(a, hooks.tplHooks())
	if err != nil {
		return nil, err
	}
	hooks.templater(t)

	t.RegisterTemplateDataFuncs(
		func(data template.FuncMap, _ string) {
			data["theme"] = general.Get()["theme"]
		},
		func(data template.FuncMap, _ string) {
			data["tz"] = timeZone
		},
		func(data template.FuncMap, page string) {
			groups, _ := json.Marshal(groupManager.Configs())
			data["groups"] = string(groups)
		},
		func(data template.FuncMap, page string) {
			monitors, _ := json.Marshal(monitorManager.MonitorsInfo())
			data["monitors"] = string(monitors)
		},
		func(data template.FuncMap, page string) {
			data["logSources"] = logger.Sources()
		},
	)
	t.RegisterTemplateDataFuncs(hooks.templateData...)

	// Routes.
	mux := http.NewServeMux()

	mux.Handle("/live", a.User(t.Render("live.tpl")))
	mux.Handle("/recordings", a.User(t.Render("recordings.tpl")))
	mux.Handle("/settings", a.User(t.Render("settings.tpl")))
	mux.Handle("/settings.js", a.User(t.Render("settings.js")))
	mux.Handle("/logs", a.Admin(t.Render("logs.tpl")))
	mux.Handle("/debug", a.Admin(t.Render("debug.tpl")))

	mux.Handle("/static/", a.User(web.Static()))
	mux.Handle("/storage/", a.User(web.Storage(env.StorageDir)))
	mux.Handle("/hls/", a.User(videoServer.HandleHLS()))

	mux.Handle("/api/system/time-zone", a.User(web.TimeZone(timeZone)))

	mux.Handle("/api/general", a.Admin(web.General(general)))
	mux.Handle("/api/general/set", a.Admin(a.CSRF(web.GeneralSet(general))))

	mux.Handle("/api/users", a.Admin(web.Users(a)))
	mux.Handle("/api/user/set", a.Admin(a.CSRF(web.UserSet(a))))
	mux.Handle("/api/user/delete", a.Admin(a.CSRF(web.UserDelete(a))))
	mux.Handle("/api/user/my-token", a.Admin(a.MyToken()))
	mux.Handle("/logout", a.Logout())

	mux.Handle("/api/monitor/list", a.User(web.MonitorList(monitorManager.MonitorsInfo)))
	mux.Handle("/api/monitor/configs", a.Admin(web.MonitorConfigs(monitorManager)))
	mux.Handle("/api/monitor/restart", a.Admin(a.CSRF(web.MonitorRestart(monitorManager))))
	mux.Handle("/api/monitor/set", a.Admin(a.CSRF(web.MonitorSet(monitorManager))))
	mux.Handle("/api/monitor/delete", a.Admin(a.CSRF(web.MonitorDelete(monitorManager))))

	mux.Handle("/api/group/configs", a.User(web.GroupConfigs(groupManager)))
	mux.Handle("/api/group/set", a.Admin(a.CSRF(web.GroupSet(groupManager))))
	mux.Handle("/api/group/delete", a.Admin(a.CSRF(web.GroupDelete(groupManager))))

	mux.Handle("/api/recording/query", a.User(web.RecordingQuery(crawler, logger)))
	mux.Handle("/api/log/feed", a.Admin(web.LogFeed(logger, a)))
	mux.Handle("/api/log/query", a.Admin(web.LogQuery(logDB)))
	mux.Handle("/api/log/sources", a.Admin(web.LogSources(logger)))
	hooks.mux(mux)

	// Main server.
	address := ":" + strconv.Itoa(env.Port)
	server := &http.Server{Addr: address, Handler: mux}

	return &App{
		log:            logger,
		logDB:          logDB,
		env:            *env,
		monitorManager: monitorManager,
		storage:        storageManager,
		videoServer:    videoServer,
		server:         server,
	}, nil
}

// App is the main application struct.
type App struct {
	log            *log.Logger
	logDB          *log.DB
	env            storage.ConfigEnv
	monitorManager *monitor.Manager
	storage        *storage.Manager
	videoServer    *video.Server
	server         *http.Server
}

func (a *App) run(ctx context.Context) error {
	if err := a.log.Start(ctx); err != nil {
		return fmt.Errorf("could not start logger: %w", err)
	}

	go a.log.LogToStdout(ctx)

	if err := a.logDB.Init(ctx); err != nil {
		// Continue even if log database is corrupt.
		time.Sleep(10 * time.Millisecond)
		a.log.Error().Src("app").Msgf("could not initialize log database: %v", err)
	} else {
		go a.logDB.SaveLogs(ctx, a.log)
		time.Sleep(10 * time.Millisecond)
	}

	a.log.Info().Src("app").Msg("Starting..")

	if err := a.env.PrepareEnvironment(); err != nil {
		return fmt.Errorf("could not prepare environment: %w", err)
	}

	if err := a.videoServer.Start(ctx); err != nil {
		return fmt.Errorf("could not start video server: %w", err)
	}

	// Start monitors.
	for _, monitor := range a.monitorManager.Monitors {
		if err := monitor.Start(); err != nil {
			a.monitorManager.StopAll()
			return fmt.Errorf("could not start monitor: %w", err)
		}
	}

	go a.storage.PurgeLoop(ctx, 10*time.Minute)

	a.log.Info().Src("app").Msgf("Serving app on port %v", a.env.Port)
	return a.server.ListenAndServe()
}
