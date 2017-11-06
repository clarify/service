// Copyright 2015 Daniel Theophanes.
// Use of this source code is governed by a zlib-style
// license that can be found in the LICENSE file.

package service

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"syscall"
	"github.com/guelfey/go.dbus"
	"text/template"
)

func isSystemd() bool {
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		return true
	}
	return false
}

type systemd struct {
	i Interface
	*Config
}

type Conn struct {
	conn *dbus.Conn
}

type Unit struct {
	c    *Conn
	path dbus.ObjectPath
}

const BusName = "org.freedesktop.systemd1"

func newSystemdService(i Interface, c *Config) (Service, error) {
	s := &systemd{
		i:      i,
		Config: c,
	}

	return s, nil
}

func (s *systemd) String() string {
	if len(s.DisplayName) > 0 {
		return s.DisplayName
	}
	return s.Name
}

// Systemd services should be supported, but are not currently.
var errNoUserServiceSystemd = errors.New("User services are not supported on systemd.")

func (s *systemd) configPath() (cp string, err error) {
	if s.Option.bool(optionUserService, optionUserServiceDefault) {
		err = errNoUserServiceSystemd
		return
	}
	cp = "/etc/systemd/system/" + s.Config.Name + ".service"
	return
}
func (s *systemd) template() *template.Template {
	return template.Must(template.New("").Funcs(tf).Parse(systemdScript))
}

func (s *systemd) Install() error {
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
		Path         string
		ReloadSignal string
		PIDFile      string
	}{
		s.Config,
		path,
		s.Option.string(optionReloadSignal, ""),
		s.Option.string(optionPIDFile, ""),
	}

	err = s.template().Execute(f, to)
	if err != nil {
		return err
	}

	err = run("sudo", "systemctl", "enable", s.Name+".service")
	if err != nil {
		return err
	}
	return run("sudo", "systemctl", "daemon-reload")
}

func (s *systemd) Uninstall() error {
	err := run("sudo", "systemctl", "disable", s.Name+".service")
	if err != nil {
		return err
	}
	cp, err := s.configPath()
	if err != nil {
		return err
	}
	if err := os.Remove(cp); err != nil {
		return err
	}
	return nil
}

func (s *systemd) Logger(errs chan<- error) (Logger, error) {
	if system.Interactive() {
		return ConsoleLogger, nil
	}
	return s.SystemLogger(errs)
}
func (s *systemd) SystemLogger(errs chan<- error) (Logger, error) {
	return newSysLogger(s.Name, errs)
}

func (s *systemd) Run() (err error) {
	err = s.i.Start(s)
	if err != nil {
		return err
	}

	s.Option.funcSingle(optionRunWait, func() {
		var sigChan = make(chan os.Signal, 3)
		signal.Notify(sigChan, syscall.SIGTERM, os.Interrupt)
		<-sigChan
	})()

	return s.i.Stop(s)
}

func (s *systemd) Start() error {
	return run("sudo", "systemctl", "start", s.Name+".service")
}

func (s *systemd) Stop() error {
	return run("sudo", "systemctl", "stop", s.Name+".service")
}

func (s *systemd) Restart() error {
	return run("sudo", "systemctl", "restart", s.Name+".service")
}

func (s *systemd) Status() (uint32, error) {
	confPath, err := s.configPath()
	if err != nil {
		return SERVICE_ERROR, err
	}

	if _, err := os.Stat(confPath); os.IsNotExist(err) {
		return SERVICE_NOT_INSTALLED, nil
	}

	conn, err := Dial()
	if err != nil {
		return SERVICE_ERROR, fmt.Errorf("DBus dial error %s", err.Error())
	}
	//defer conn.Close()

	unit, err := conn.UnitByName(s.Name + ".service")
	if err != nil {
		return SERVICE_ERROR, fmt.Errorf("DBus unit error %s", err.Error())
	}

	load, active, sub, err := unit.State()
	if err != nil {
		return SERVICE_ERROR, fmt.Errorf("DBus unit state error %s", err.Error())
	}

	//  loaded, error, masked
	if load == "error" {
		return SERVICE_ERROR, nil
	} else if load == "loaded" {
		//active, reloading, inactive, failed, activating, deactivating
		switch active {
		case "active":
			// rinning, exited. Not documented part.
			if sub == "running" {
				return SERVICE_RUNNING, nil
			} else if sub == "exited" {
				return SERVICE_STOPPED, nil
			} else {
				return SERVICE_ERROR, fmt.Errorf("Unknown service sub-state given: %s", sub)
			}
		case "reloading":
			return SERVICE_START_PENDING, nil
		case "inactive":
			return SERVICE_STOPPED, nil
		case "failed":
			return SERVICE_ERROR, nil
		case "activating":
			return SERVICE_START_PENDING, nil
		case "deactivating":
			return SERVICE_STOP_PENDING, nil
		}
	} else {
		return SERVICE_ERROR, fmt.Errorf("Service %s is marked as masked", s.Name)
	}

	return SERVICE_ERROR, fmt.Errorf("Couldn't get service state")
}

func userAndHome() (string, string, error) {
	u, err := user.Current()
	if err != nil {
		out, nerr := exec.Command("sh", "-c", "getent passwd `id -u`").Output()

		if nerr != nil {
			return "", "", err
		}

		fields := bytes.Split(out, []byte(`:`))
		if len(fields) >= 6 {
			return string(fields[0]), string(fields[5]), nil
		}

		return "", "", fmt.Errorf("Unable to figure out the home dir")
	}

	return u.Username, u.HomeDir, nil
}

func Dial() (*Conn, error) {
	conn, err := dbus.SystemBusPrivate()
	if err != nil {
		return nil, err
	}

	user, home, err := userAndHome()
	if err != nil {
		return nil, err
	}

	methods := []dbus.Auth{dbus.AuthExternal(user), dbus.AuthCookieSha1(user, home)}
	if err = conn.Auth(methods); err != nil {
		conn.Close()
		return nil, err
	}

	if err = conn.Hello(); err != nil {
		conn.Close()
		conn = nil
		return nil, fmt.Errorf("Unable to perform a handshake")
	}

	return &Conn{conn}, nil
}

func (c *Conn) Close() error {
	return c.conn.Close()
}

func (c *Conn) object(path dbus.ObjectPath) *dbus.Object {
	return c.conn.Object(BusName, path)
}

func (c *Conn) UnitByName(name string) (*Unit, error) {
	obj := c.object("/org/freedesktop/systemd1")

	var s dbus.ObjectPath
	err := obj.Call("org.freedesktop.systemd1.Manager.LoadUnit", 0, name).Store(&s)
	if err != nil {
		return nil, err
	}

	return &Unit{c, s}, nil
}

func (u *Unit) obj() *dbus.Object {
	return u.c.object(u.path)
}

func (u *Unit) State() (load, active, sub string, err error) {
	val, err := u.obj().GetProperty("org.freedesktop.systemd1.Unit.LoadState")
	if err != nil {
		return
	}

	if l, ok := val.Value().(string); ok {
		load = l
	} else {
		err = fmt.Errorf("Unable to get load state")
		return
	}

	val, err = u.obj().GetProperty("org.freedesktop.systemd1.Unit.ActiveState")
	if err != nil {
		return
	}

	if act, ok := val.Value().(string); ok {
		active = act
	} else {
		err = fmt.Errorf("Unable to get active state")
		return
	}

	val, err = u.obj().GetProperty("org.freedesktop.systemd1.Unit.SubState")
	if err != nil {
		return
	}

	if s, ok := val.Value().(string); ok {
		sub = s
	} else {
		err = fmt.Errorf("Unable to get sub-state")
		return
	}

	err = nil
	return
}

const systemdScript = `[Unit]
Description={{.Description}}
ConditionFileIsExecutable={{.Path|cmdEscape}}

[Service]
StartLimitInterval=5
StartLimitBurst=10
ExecStart={{.Path|cmdEscape}}{{range .Arguments}} {{.|cmd}}{{end}}
{{if .ChRoot}}RootDirectory={{.ChRoot|cmd}}{{end}}
{{if .WorkingDirectory}}WorkingDirectory={{.WorkingDirectory|cmdEscape}}{{end}}
{{if .UserName}}User={{.UserName}}{{end}}
{{if .ReloadSignal}}ExecReload=/bin/kill -{{.ReloadSignal}} "$MAINPID"{{end}}
{{if .PIDFile}}PIDFile={{.PIDFile|cmd}}{{end}}
Restart=always
RestartSec=120
EnvironmentFile=-/etc/sysconfig/{{.Name}}

[Install]
WantedBy=multi-user.target
`
