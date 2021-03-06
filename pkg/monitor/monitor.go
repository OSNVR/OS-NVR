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

package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"nvr/pkg/ffmpeg"
	"nvr/pkg/log"
	"nvr/pkg/storage"
	"nvr/pkg/video"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// StartHook is called when monitor start.
type StartHook func(context.Context, *Monitor)

// StartInputHook is called when input process start.
type StartInputHook func(context.Context, *InputProcess, *[]string)

// EventHook is called on every event.
type EventHook func(*Monitor, *storage.Event)

// RecSaveHook is called when recording is saved.
type RecSaveHook func(*Monitor, *string)

// RecSavedHook is called after recording have been saved successfully.
type RecSavedHook func(*Monitor, string, storage.RecordingData)

// Hooks monitor hooks.
type Hooks struct {
	Start      StartHook
	StartInput StartInputHook
	Event      EventHook
	RecSave    RecSaveHook
	RecSaved   RecSavedHook
}

// Configs Monitor configurations.
type Configs map[string]Config

// Config Monitor configuration.
type Config map[string]string

func (c Config) enabled() bool {
	return c["enable"] == "true"
}

// ID returns id of monitor.
func (c Config) ID() string {
	return c["id"]
}

// Name returns name of monitor.
func (c Config) Name() string {
	return c["name"]
}

func (c Config) audioEnabled() bool {
	switch c["audioEncoder"] {
	case "":
		return false
	case "none":
		return false
	}
	return true
}

// MainInput main input url.
func (c Config) MainInput() string {
	return c["mainInput"]
}

// SubInput sub input url.
func (c Config) SubInput() string {
	return c["subInput"]
}

// SubInputEnabled if sub input is available.
func (c Config) SubInputEnabled() bool {
	return c.SubInput() != ""
}

func (c Config) videoLength() string {
	return c["videoLength"]
}

// LogLevel getter.
func (c Config) LogLevel() string {
	return c["logLevel"]
}

// Hwacell getter.
func (c Config) Hwacell() string {
	return c["hwaccel"]
}

// Manager for the monitors.
type Manager struct {
	Monitors    monitors
	env         storage.ConfigEnv
	log         *log.Logger
	videoServer *video.Server
	path        string
	hooks       Hooks
	mu          sync.Mutex
}

// NewManager return new monitor manager.
func NewManager(
	configPath string,
	env storage.ConfigEnv,
	log *log.Logger,
	videoServer *video.Server,
	hooks *Hooks,
) (*Manager, error) {
	if err := os.MkdirAll(configPath, 0o700); err != nil {
		return nil, fmt.Errorf("create monitors directory: %w", err)
	}

	configFiles, err := readConfigs(configPath)
	if err != nil {
		return nil, fmt.Errorf("read configuration files: %w", err)
	}

	manager := &Manager{
		env:         env,
		log:         log,
		videoServer: videoServer,
		path:        configPath,
		hooks:       *hooks,
	}

	monitors := make(monitors)
	for _, file := range configFiles {
		var config Config
		if err := json.Unmarshal(file, &config); err != nil {
			return nil, fmt.Errorf("unmarshal config: %w: %v", err, file)
		}
		monitors[config["id"]] = manager.newMonitor(config)
	}
	manager.Monitors = monitors

	return manager, nil
}

func readConfigs(path string) ([][]byte, error) {
	var files [][]byte
	fileSystem := os.DirFS(path)
	err := fs.WalkDir(fileSystem, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.Contains(path, ".json") {
			return nil
		}
		file, err := fs.ReadFile(fileSystem, path)
		if err != nil {
			return fmt.Errorf("read file: %v %w", path, err)
		}
		files = append(files, file)
		return nil
	})
	return files, err
}

// MonitorSet sets config for specified monitor.
func (m *Manager) MonitorSet(id string, c Config) error {
	defer m.mu.Unlock()
	m.mu.Lock()

	monitor, exist := m.Monitors[id]
	if exist {
		monitor.Mu.Lock()
		monitor.Config = c
		monitor.Mu.Unlock()
	} else {
		monitor = m.newMonitor(c)
		m.Monitors[id] = monitor
	}

	// Update file.
	monitor.Mu.Lock()
	config, _ := json.MarshalIndent(monitor.Config, "", "    ")

	if err := os.WriteFile(m.configPath(id), config, 0o600); err != nil {
		return err
	}
	monitor.Mu.Unlock()

	return nil
}

// ErrNotExist monitor does not exist.
var ErrNotExist = errors.New("monitor does not exist")

// MonitorDelete deletes monitor by id.
func (m *Manager) MonitorDelete(id string) error {
	defer m.mu.Unlock()
	m.mu.Lock()
	monitors := m.Monitors

	monitor, exists := monitors[id]
	if !exists {
		return ErrNotExist
	}
	monitor.Stop()

	delete(m.Monitors, id)

	if err := os.Remove(m.configPath(id)); err != nil {
		return err
	}

	return nil
}

// MonitorsInfo returns common information about the monitors.
// This will be accessesable by normal users.
func (m *Manager) MonitorsInfo() Configs {
	configs := make(map[string]Config)
	m.mu.Lock()
	for _, monitor := range m.Monitors {
		monitor.Mu.Lock()
		c := monitor.Config
		monitor.Mu.Unlock()

		enable := "false"
		if c.enabled() {
			enable = "true"
		}

		audioEnabled := "false"
		if c.audioEnabled() {
			audioEnabled = "true"
		}

		subInputEnabled := "false"
		if c.SubInputEnabled() {
			subInputEnabled = "true"
		}

		configs[c.ID()] = Config{
			"id":              c.ID(),
			"name":            c.Name(),
			"enable":          enable,
			"audioEnabled":    audioEnabled,
			"subInputEnabled": subInputEnabled,
		}
	}
	m.mu.Unlock()
	return configs
}

func (m *Manager) configPath(id string) string {
	return m.path + "/" + id + ".json"
}

// MonitorConfigs returns configurations for all monitors.
func (m *Manager) MonitorConfigs() map[string]Config {
	configs := make(map[string]Config)

	m.mu.Lock()
	for _, monitor := range m.Monitors {
		monitor.Mu.Lock()
		configs[monitor.Config.ID()] = monitor.Config
		monitor.Mu.Unlock()
	}
	m.mu.Unlock()

	return configs
}

func (m *Manager) newMonitor(config Config) *Monitor {
	monitor := &Monitor{
		Env:         m.env,
		videoServer: m.videoServer,
		Log:         m.log,
		Config:      config,

		eventsMu:  sync.Mutex{},
		eventChan: make(chan storage.Event),

		hooks:               m.hooks,
		runRecordingProcess: runRecordingProcess,
		NewProcess:          ffmpeg.NewProcess,
		videoDuration:       ffmpeg.New(m.env.FFmpegBin).VideoDuration,

		WG: &sync.WaitGroup{},
	}
	monitor.mainInput = monitor.newInputProcess(false)
	monitor.subInput = monitor.newInputProcess(true)

	return monitor
}

// monitors map.
type monitors map[string]*Monitor

// Monitor service.
type Monitor struct {
	Env         storage.ConfigEnv
	videoServer *video.Server
	Log         *log.Logger
	Config      Config

	events    storage.Events
	eventsMu  sync.Mutex
	eventChan chan storage.Event

	running bool

	mainInput *InputProcess
	subInput  *InputProcess

	hooks               Hooks
	runRecordingProcess runRecordingProcessFunc
	NewProcess          ffmpeg.NewProcessFunc
	videoDuration       ffmpeg.VideoDurationFunc

	Mu     sync.Mutex
	WG     *sync.WaitGroup
	cancel func()
}

// ErrRunning monitor is already running.
var ErrRunning = errors.New("monitor is aleady running")

// Start monitor.
func (m *Monitor) Start() error {
	defer m.Mu.Unlock()
	m.Mu.Lock()
	if m.running {
		return ErrRunning
	}
	m.running = true

	id := m.Config.ID()

	if !m.Config.enabled() {
		m.Log.Info().Src("monitor").Monitor(id).Msg("disabled")
		return nil
	}

	m.Log.Info().Src("monitor").Monitor(id).Msg("starting")

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	if m.alwaysRecord() {
		infinte := time.Duration(1<<63 - 62135596801)
		go func() {
			select {
			case <-ctx.Done():
			case <-time.After(15 * time.Second):
				err := m.SendEvent(storage.Event{
					Time:        time.Now(),
					RecDuration: infinte,
				})
				if err != nil {
					m.Log.Error().
						Src("monitor").Monitor(id).
						Msgf("could not start continuous recording: %v", err)
				}
			}
		}()
	}

	m.hooks.Start(ctx, m)

	m.WG.Add(1)
	go m.mainInput.start(ctx, m)

	if m.Config.SubInputEnabled() {
		m.WG.Add(1)
		go m.subInput.start(ctx, m)
	}

	m.WG.Add(1)
	go m.startRecorder(ctx)

	return nil
}

func (m *Monitor) newInputProcess(isSubInput bool) *InputProcess {
	i := &InputProcess{
		isSubInput:       isSubInput,
		M:                m,
		runInputProcess:  runInputProcess,
		sizeFromStream:   ffmpeg.New(m.Env.FFmpegBin).SizeFromStream,
		newProcess:       ffmpeg.NewProcess,
		watchdogInterval: 10 * time.Second,
	}

	return i
}

type runInputProcessFunc func(context.Context, *InputProcess) error

// InputProcess monitor input process.
type InputProcess struct {
	isSubInput   bool
	hlsAddress   string
	rtspAddress  string
	rtspProtocol string

	// Stream size.
	width  int
	height int

	waitForNewHLSsegment video.WaitForNewHLSsegementFunc
	cancel               func()

	M *Monitor

	runInputProcess  runInputProcessFunc
	sizeFromStream   ffmpeg.SizeFromStreamFunc
	newProcess       ffmpeg.NewProcessFunc
	watchdogInterval time.Duration
}

// IsSubInput if the input is the sub stream.
func (i *InputProcess) IsSubInput() bool {
	return i.isSubInput
}

// HLSaddress internal HLS address.
func (i *InputProcess) HLSaddress() string {
	return i.hlsAddress
}

// RTSPaddress internal RTSP address.
func (i *InputProcess) RTSPaddress() string {
	return i.rtspAddress
}

// RTSPprotocol protocol used by RTSP address.
func (i *InputProcess) RTSPprotocol() string {
	return i.rtspProtocol
}

// Width stream width.
func (i *InputProcess) Width() int {
	return i.width
}

// Height stream height.
func (i *InputProcess) Height() int {
	return i.height
}

// ProcessName name of process "main" or "sub".
func (i *InputProcess) ProcessName() string {
	if i.isSubInput {
		return "sub"
	}
	return "main"
}

func (i *InputProcess) input() string {
	if i.IsSubInput() {
		return i.M.Config.SubInput()
	}
	return i.M.Config.MainInput()
}

func (i *InputProcess) rtspPathName() string {
	id := i.M.Config.ID()
	if i.isSubInput {
		return id + "_sub"
	}
	return id
}

// WaitForNewHLSsegment waits for a new HLS segment and
// returns the combined duration of the last nSegments.
// Used to calculate start time of the recordings.
func (i *InputProcess) WaitForNewHLSsegment(
	ctx context.Context, nSegments int,
) (time.Duration, error) {
	return i.waitForNewHLSsegment(ctx, nSegments)
}

// Cancel process context.
func (i *InputProcess) Cancel() {
	i.cancel()
}

func (i *InputProcess) start(ctx context.Context, m *Monitor) {
	for {
		if ctx.Err() != nil {
			m.Log.Info().
				Src("monitor").
				Monitor(i.M.Config.ID()).
				Msgf("%v process: stopped", i.ProcessName())

			m.WG.Done()

			return
		}

		if err := i.runInputProcess(ctx, i); err != nil {
			m.Log.Error().
				Src("monitor").
				Monitor(i.M.Config.ID()).
				Msgf("%v process: crashed: %v", i.ProcessName(), err)

			select {
			case <-ctx.Done():
			case <-time.After(1 * time.Second):
			}
			continue
		}
	}
}

func runInputProcess(ctx context.Context, i *InputProcess) error {
	id := i.M.Config.ID()

	pathConf := video.PathConf{MonitorID: id, IsSub: i.IsSubInput()}

	hlsAddress, rtspAddress, rtspProtocol, waitForNewHLSsegment, cancel, err := i.M.videoServer.NewPath(i.rtspPathName(), pathConf) //nolint:lll
	if err != nil {
		return fmt.Errorf("add path to RTSP server: %w", err)
	}
	defer cancel()

	i.hlsAddress = hlsAddress
	i.rtspAddress = rtspAddress
	i.rtspProtocol = rtspProtocol
	i.waitForNewHLSsegment = waitForNewHLSsegment

	inputOpts := i.M.Config["inputOptions"]
	i.width, i.height, err = i.sizeFromStream(ctx, inputOpts, i.input())
	if err != nil {
		return fmt.Errorf("get size of stream: %w", err)
	}

	processCTX, cancel2 := context.WithCancel(ctx)
	i.cancel = cancel2
	defer cancel2()

	args := ffmpeg.ParseArgs(i.generateArgs())

	i.M.hooks.StartInput(processCTX, i, &args)

	cmd := exec.Command(i.M.Env.FFmpegBin, args...)

	logFunc := func(msg string) {
		i.M.Log.FFmpegLevel(i.M.Config.LogLevel()).
			Src("monitor").
			Monitor(id).
			Msgf("%v process: %v", i.ProcessName(), msg)
	}

	process := i.newProcess(cmd).
		Timeout(10 * time.Second).
		StdoutLogger(logFunc).
		StderrLogger(logFunc)

	i.M.Log.Info().
		Src("monitor").
		Monitor(id).
		Msgf("starting %v process: %v", i.ProcessName(), cmd)

	err = process.Start(processCTX) // Blocks until process exits.
	if err != nil {
		return fmt.Errorf("crashed: %w", err)
	}

	return nil
}

func (i *InputProcess) generateArgs() string {
	// OUTPUT
	// -threads 1 -loglevel error -hwaccel x -i rtsp://x -c:a aac -c:v libx264
	// -f rtsp -rtsp_transport tcp rtsp://127.0.0.1:2021/test

	c := i.M.Config

	var args string

	args += "-threads 1 -loglevel " + c.LogLevel()
	if c.Hwacell() != "" {
		args += " -hwaccel " + c.Hwacell()
	}
	if i.M.Config["inputOptions"] != "" {
		args += " " + i.M.Config["inputOptions"]
	}
	args += " -i " + i.input()

	if i.M.Config.audioEnabled() {
		args += " -c:a " + c["audioEncoder"]
	} else {
		args += " -an" // Skip audio.
	}

	args += " -c:v " + c["videoEncoder"]
	args += " -f rtsp -rtsp_transport " + i.RTSPprotocol() + " " + i.RTSPaddress()

	return args
}

// SendEventFunc send event signature.
type SendEventFunc func(storage.Event) error

// SendEvent sends event to monitor.
func (m *Monitor) SendEvent(event storage.Event) error {
	m.Mu.Lock()
	if !m.running {
		m.Mu.Unlock()
		return context.Canceled
	}
	m.Mu.Unlock()

	if err := event.Validate(); err != nil {
		return fmt.Errorf("invalid event: %w", err)
	}
	m.eventChan <- event
	return nil
}

// Stop monitor.
func (m *Monitor) Stop() {
	m.Mu.Lock()
	m.running = false
	m.Mu.Unlock()

	if m.cancel != nil {
		m.cancel()
	}
	m.WG.Wait()
}

// StopAll monitors.
func (m *Manager) StopAll() {
	m.mu.Lock()
	for _, monitor := range m.Monitors {
		monitor.Stop()
	}
	m.mu.Unlock()
}

func (m *Monitor) alwaysRecord() bool {
	return m.Config["alwaysRecord"] == "true"
}
