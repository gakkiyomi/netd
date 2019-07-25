package conn

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/sky-cloud-tec/netd/cli"
	"github.com/sky-cloud-tec/netd/protocol"
	"github.com/songtianyi/rrframework/logs"

	"github.com/sky-cloud-tec/netd/common"
	"golang.org/x/crypto/ssh"

	"github.com/ziutek/telnet"
)

var (
	conns map[string]*CliConn
	semas map[string]chan struct{}
)

func init() {
	conns = make(map[string]*CliConn, 0)
	semas = make(map[string]chan struct{}, 0)
}

// CliConn cli connection
type CliConn struct {
	t    int                  // connection type 0 = ssh, 1 = telnet
	mode string               // device cli mode
	req  *protocol.CliRequest // cli request
	op   cli.Operator         // cli operator

	conn   *telnet.Conn // telnet connection
	client *ssh.Client  // ssh client

	session *ssh.Session   // ssh session
	r       io.Reader      // ssh session stdout
	w       io.WriteCloser // ssh session stdin
}

// Acquire cli conn
func Acquire(req *protocol.CliRequest, op cli.Operator) (*CliConn, error) {
	// limit concurrency to 1
	// there only one req for one connection always
	logs.Debug(req.LogPrefix, "Acquiring sema...")
	if semas[req.Address] == nil {
		semas[req.Address] = make(chan struct{}, 1)
	}
	// try
	semas[req.Address] <- struct{}{}
	logs.Debug(req.LogPrefix, "sema acquired")
	// if cli conn already created
	if v, ok := conns[req.Address]; ok {
		v.req = req
		v.op = op
		logs.Debug(req.LogPrefix, "cli conn exist")
		return v, nil
	}
	c, err := newCliConn(req, op)
	if err != nil {
		return nil, err
	}
	conns[req.Address] = c
	return c, nil
}

// Release cli conn
func Release(req *protocol.CliRequest) {
	if len(semas[req.Address]) > 0 {
		logs.Debug(req.LogPrefix, "Releasing sema")
		<-semas[req.Address]
	}
	logs.Debug(req.LogPrefix, "sema released")
}

func newCliConn(req *protocol.CliRequest, op cli.Operator) (*CliConn, error) {
	logs.Debug(req.LogPrefix, "creating cli conn...")
	if strings.ToLower(req.Protocol) == "ssh" {
		sshConfig := &ssh.ClientConfig{
			User:            req.Auth.Username,
			Auth:            []ssh.AuthMethod{ssh.Password(req.Auth.Password)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout:         5 * time.Second,
		}
		sshConfig.SetDefaults()
		sshConfig.Ciphers = append(sshConfig.Ciphers, "aes128-cbc")
		client, err := ssh.Dial("tcp", req.Address, sshConfig)
		if err != nil {
			logs.Error(req.LogPrefix, "dial", req.Address, "error", err)
			return nil, fmt.Errorf("%s dial %s error, %s", req.LogPrefix, req.Address, err)
		}
		c := &CliConn{t: common.SSHConn, client: client, req: req, op: op, mode: "login"}
		if err := c.init(); err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	} else if strings.ToLower(req.Protocol) == "telnet" {
		conn, err := telnet.DialTimeout("tcp", req.Address, 5*time.Second)
		if err != nil {
			return nil, fmt.Errorf("[ %s ] dial %s error, %s", req.Device, req.Address, err)
		}
		c := &CliConn{t: common.TELNETConn, conn: conn, req: req, op: op, mode: "login"}
		return c, nil
	}
	return nil, fmt.Errorf("protocol %s not support", req.Protocol)
}

func (s *CliConn) heartbeat() {
	go func() {
		tick := time.Tick(30 * time.Second)
		for {
			select {
			case <-tick:
				// try
				logs.Info(s.req.LogPrefix, "Acquiring heartbeat sema...")
				semas[s.req.Address] <- struct{}{}
				logs.Info(s.req.LogPrefix, "heartbeat sema acquired")
				if _, err := s.writeBuff(""); err != nil {
					semas[s.req.Address] <- struct{}{}
					logs.Critical(s.req.LogPrefix, "heartbeat error,", err)
					s.Close()
					return
				}
				if _, _, err := s.readBuff(); err != nil {
					semas[s.req.Address] <- struct{}{}
					logs.Critical(s.req.LogPrefix, "heartbeat error,", err)
					s.Close()
					return
				}
				<-semas[s.req.Address]
			}
		}
	}()
}

func (s *CliConn) init() error {
	if s.t == common.SSHConn {
		f := s.op.GetSSHInitializer()
		var err error
		s.r, s.w, s.session, err = f(s.client)
		if err != nil {
			return err
		}

		// read login prompt
		if _, _, err := s.readBuff(); err != nil {
			return fmt.Errorf("read after login failed, %s", err)
		}
	}
	s.heartbeat()
	return nil
}

// Close cli conn
func (s *CliConn) Close() error {
	delete(conns, s.req.Address)
	if s.t == common.TELNETConn {
		if s.conn == nil {
			logs.Info("telnet conn nil when close")
			return nil
		}
		return s.conn.Close()
	}
	if s.session != nil {
		if err := s.session.Close(); err != nil {
			return err
		}
	} else {
		logs.Notice("ssh session nil when close")
	}
	if s.client == nil {
		logs.Notice("ssh conn nil when close")
		return nil
	}
	return s.client.Close()
}

func (s *CliConn) read(buff []byte) (int, error) {
	if s.t == common.SSHConn {
		return s.r.Read(buff)
	}
	return s.conn.Read(buff)
}

func (s *CliConn) write(b []byte) (int, error) {
	if s.t == common.SSHConn {
		return s.w.Write(b)
	}
	return s.conn.Write(b)
}

type readBuffOut struct {
	err    error
	ret    string
	prompt string
}

func (s *CliConn) findLastLine(t string) string {
	scanner := bufio.NewScanner(strings.NewReader(t))
	var last string
	for scanner.Scan() {
		last = scanner.Text()
	}
	return last
}

// AnyPatternMatches return matched string slice if any pattern fullfil
func (s *CliConn) anyPatternMatches(t string, patterns []*regexp.Regexp) []string {
	for _, v := range patterns {
		matches := v.FindStringSubmatch(t)
		logs.Debug(v, t, matches)
		if len(matches) != 0 {
			return matches
		}
	}
	return nil
}

func (s *CliConn) readLines() *readBuffOut {
	buf := make([]byte, 1000)
	var (
		waitingString, lastLine string
	)
	for {
		n, err := s.read(buf) //this reads the ssh/telnet terminal
		if err != nil {
			// something wrong
			logs.Error(s.req.LogPrefix, "io.Reader read error,", err)
			break
		}
		// for every line
		current := string(buf[:n])
		logs.Debug(s.req.LogPrefix, "(", n, ")", current)
		lastLine = s.findLastLine(waitingString + current)
		logs.Debug("lastline:", lastLine, ":")
		matches := s.anyPatternMatches(lastLine, s.op.GetPrompts(s.mode))
		if len(matches) > 0 {
			logs.Info(s.req.LogPrefix, "[prompt matched]", matches)
			waitingString = strings.TrimSuffix(waitingString+current, matches[0])
			break
		}
		// add current line to result string
		waitingString += current
	}
	return &readBuffOut{
		nil,
		waitingString,
		lastLine,
	}
}

// return cmd output, prompt, error
func (s *CliConn) readBuff() (string, string, error) {
	// buffered chan
	ch := make(chan *readBuffOut, 1)

	go func() {
		ch <- s.readLines()
	}()

	select {
	case res := <-ch:
		if res.err == nil {
			scanner := bufio.NewScanner(strings.NewReader(res.ret))
			for scanner.Scan() {
				matches := s.anyPatternMatches(scanner.Text(), s.op.GetErrPatterns())
				if len(matches) > 0 {
					logs.Info(s.req.LogPrefix, "err pattern matched,", matches)
					return "", res.prompt, fmt.Errorf("err pattern matched, %s", matches)
				}
			}
		}
		return res.ret, res.prompt, res.err
	case <-time.After(s.req.Timeout):
		return "", "", fmt.Errorf("read stdout timeout after %q", s.req.Timeout)
	}
}

func (s *CliConn) writeBuff(cmd string) (int, error) {
	return s.write([]byte(cmd + s.op.GetLinebreak()))
}

// Exec execute cli cmds
func (s *CliConn) Exec() (map[string]string, error) {
	// transit to target mode
	if s.req.Mode != s.mode {
		cmds := s.op.GetTransitions(s.mode, s.req.Mode)
		// use target mode prompt
		s.mode = s.req.Mode
		for _, v := range cmds {
			if _, err := s.writeBuff(v); err != nil {
				logs.Error(s.req.LogPrefix, "write buff failed,", err)
				return nil, fmt.Errorf("write buff failed, %s", err)
			}
			_, _, err := s.readBuff()
			if err != nil {
				logs.Error(s.req.LogPrefix, "readBuff failed,", err)
				return nil, fmt.Errorf("readBuff failed, %s", err)
			}
		}
	}
	cmdstd := make(map[string]string, 0)
	// do execute cli commands
	for _, v := range s.req.Commands {
		if _, err := s.writeBuff(v); err != nil {
			logs.Error(s.req.LogPrefix, "write buff failed,", err)
			return cmdstd, fmt.Errorf("write buff failed, %s", err)
		}
		ret, _, err := s.readBuff()
		if err != nil {
			logs.Error(s.req.LogPrefix, "readBuff failed,", err)
			return cmdstd, fmt.Errorf("readBuff failed, %s", err)
		}
		cmdstd[v] = ret
	}
	return cmdstd, nil
}