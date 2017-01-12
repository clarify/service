// Copyright 2015 Daniel Theophanes.
// Use of this source code is governed by a zlib-style
// license that can be found in the LICENSE file.

package service

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"text/template"
	"time"
	upstartmgr "github.com/vektra/tachyon/upstart"
)

var startState map[string]uint32
var stopState  map[string]uint32

func isUpstart() bool {
	if _, err := os.Stat("/sbin/upstart-udev-bridge"); err == nil {
		return true
	}
	return false
}

type upstart struct {
	i Interface
	*Config
}

func newUpstartService(i Interface, c *Config) (Service, error) {
	s := &upstart{
		i:      i,
		Config: c,
	}

	startState = map[string]uint32{
		"running":    SERVICE_RUNNING,
		"starting":   SERVICE_START_PENDING,
		"pre-start":  SERVICE_START_PENDING,
		"post-start": SERVICE_START_PENDING,
	}

	stopState = map[string]uint32{
		"waiting":   SERVICE_STOPPED,
		"stopping":  SERVICE_STOP_PENDING,
		"pre-stop":  SERVICE_STOP_PENDING,
		"post-stop": SERVICE_STOP_PENDING,
	}

	return s, nil
}

func (s *upstart) String() string {
	if len(s.DisplayName) > 0 {
		return s.DisplayName
	}
	return s.Name
}

// Upstart has some support for user services in graphical sessions.
// Due to the mix of actual support for user services over versions, just don't bother.
// Upstart will be replaced by systemd in most cases anyway.
var errNoUserServiceUpstart = errors.New("User services are not supported on Upstart.")

func (s *upstart) configPath() (cp string, err error) {
	if s.Option.bool(optionUserService, optionUserServiceDefault) {
		err = errNoUserServiceUpstart
		return
	}
	cp = "/etc/init/" + s.Config.Name + ".conf"
	return
}
func (s *upstart) template() *template.Template {
	return template.Must(template.New("").Funcs(tf).Parse(upstartScript))
}

func (s *upstart) Install() error {
	confPath, err := s.configPath()
	if err != nil {
		return err
	}
	_, err = os.Stat(confPath)
	if err == nil {
		return fmt.Errorf("Init already exists: %s", confPath)
	}

	f, err := os.Create(confPath)
	if err != nil {
		return err
	}
	defer f.Close()

	path, err := s.execPath()
	if err != nil {
		return err
	}

	var to = &struct {
		*Config
		Path string
	}{
		s.Config,
		path,
	}

	return s.template().Execute(f, to)
}

func (s *upstart) Uninstall() error {
	cp, err := s.configPath()
	if err != nil {
		return err
	}
	if err := os.Remove(cp); err != nil {
		return err
	}
	return nil
}

func (s *upstart) Logger(errs chan<- error) (Logger, error) {
	if system.Interactive() {
		return ConsoleLogger, nil
	}
	return s.SystemLogger(errs)
}
func (s *upstart) SystemLogger(errs chan<- error) (Logger, error) {
	return newSysLogger(s.Name, errs)
}

func (s *upstart) Run() (err error) {
	err = s.i.Start(s)
	if err != nil {
		return err
	}

	s.Option.funcSingle(optionRunWait, func() {
		var sigChan = make(chan os.Signal, 3)
		signal.Notify(sigChan, os.Interrupt, os.Kill)
		<-sigChan
	})()

	return s.i.Stop(s)
}

func (s *upstart) Start() error {
	return run("initctl", "start", s.Name)
}

func (s *upstart) Stop() error {
	return run("initctl", "stop", s.Name)
}

func (s *upstart) Restart() error {
	err := s.Stop()
	if err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	return s.Start()
}

func (s *upstart) Status() (uint32, error) {
	confPath, err := s.configPath()
	if err != nil {
		return SERVICE_ERROR, err
	}

	if _, err := os.Stat(confPath); os.IsNotExist(err) {
		return SERVICE_NOT_INSTALLED, nil
	}

	conn, err := upstartmgr.Dial()
	if err != nil {
		return SERVICE_ERROR, fmt.Errorf("Upstart dial error %s", err.Error())
	}
	//defer conn.Close()

	j, err := conn.Job(s.Name)
	if err != nil {
		return SERVICE_ERROR, fmt.Errorf("Upstart job error %s", err.Error())
	}

	is, err := j.Instances()
	if err != nil {
		return SERVICE_ERROR, fmt.Errorf("Upstart instances error %s", err.Error())
	}

	if is == nil || len(is) == 0 {
		return SERVICE_STOPPED, nil
	}

	state, err := is[0].State()
	if err != nil {
		return SERVICE_ERROR, fmt.Errorf("Instances state error %s", err.Error())
	}

	goal, err := is[0].Goal()
	if err != nil {
		return SERVICE_ERROR, fmt.Errorf("Instance goal error %s", err.Error())
	}

	switch goal {
	case "start":
		s, found := startState[state]
		if found {
			return s, nil
		} else {
			return SERVICE_RUNNING, nil
		}

	case "stop":
		s, found := stopState[state]
		if found {
			return s, nil
		} else {
			return SERVICE_STOPPED, nil
		}
	}

	return SERVICE_ERROR, nil
}

// The upstart script should stop with an INT or the Go runtime will terminate
// the program before the Stop handler can run.
const upstartScript = `# {{.Description}}

 {{if .DisplayName}}description    "{{.DisplayName}}"{{end}}

kill signal INT
{{if .ChRoot}}chroot {{.ChRoot}}{{end}}
{{if .WorkingDirectory}}chdir {{.WorkingDirectory}}{{end}}
start on filesystem or runlevel [2345]
stop on runlevel [!2345]

{{if .UserName}}setuid {{.UserName}}{{end}}

respawn
respawn limit 10 5
umask 022

console none

pre-start script
    test -x {{.Path}} || { stop; exit 0; }
end script

# Start
exec {{.Path}}{{range .Arguments}} {{.|cmd}}{{end}}
`
