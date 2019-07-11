package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	//	"syscall"
	"bytes"
	"errors"
	"strings"
	//	"net/http"

	"github.com/vishvananda/netns"
)

var (
	errConnClosed = errors.New("the persistent connection closed")
)

func mustOpen(name string) *os.File {
	f, err := os.Open(name)
	if err != nil {
		panic(err)
	}
	return f
}

var (
	devNull = mustOpen(os.DevNull)
)

type dialResp struct {
	net.Conn
	error
}

type dialReq struct {
	network string
	address string
	resch   chan<- dialResp
}

type pinnedDialer struct {
	/* read in the loop, written in Dial */
	reqch chan dialReq

	/* waited on anywhere, closed in loop/Quit */
	readych chan interface{}
	quitch  chan interface{}
}

func CreatePinnedDialer() *pinnedDialer {
	return &pinnedDialer{
		make(chan dialReq),
		make(chan interface{}),
		make(chan interface{}),
	}
}

func (d *pinnedDialer) loop() {
	var req dialReq
	var resp dialResp

	close(d.readych)

	for {
		select {
		case <-d.quitch:
			return
		case req = <-d.reqch:
		}

		resp.Conn, resp.error = net.Dial(req.network, req.address)
		req.resch <- resp
	}
}

func (d *pinnedDialer) WaitReady() <-chan interface{} {
	ret := make(chan interface{})

	go func() {
		select {
		case <-d.quitch:
		case <-d.readych:
		}

		close(ret)
	}()

	return ret
}

func (d *pinnedDialer) Quit() {
	close(d.quitch)
}

func (d *pinnedDialer) Dial(network, address string) (net.Conn, error) {
	resch := make(chan dialResp)
	req := dialReq{
		network: network,
		address: address,
		resch:   resch,
	}

	select {
	case <-d.quitch:
		return nil, errConnClosed
	case d.reqch <- req:
	}

	res, ok := <-resch
	if !ok {
		return nil, errConnClosed
	}
	return res.Conn, res.error
}

var hostIfCounter int = 0

func allocHostIfName() (name string) {
	name = fmt.Sprintf("ve-envdeploy%d", hostIfCounter)
	hostIfCounter += 1
	return
}

func runContainedWithDialerThread(id string, cmd string, env []string, dir string, cgroupPath string, stderr *os.File, pd *pinnedDialer, donech chan<- interface{}) {
	var proc *os.Process
	var state *os.ProcessState
	var err error
	var path string
	var argv []string

	bgNetns, _ := netns.Get()

	donechClosed := false
	done := func() {
		if donechClosed {
			return
		}
		donechClosed = true
		close(donech)
	}
	defer done()

	argv = strings.Split(cmd, " ")
	if len(argv) == 0 {
		fmt.Fprintf(stderr, "envdeploy: no command to run\n")
		return
	}
	path, err = exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintf(stderr, "envdeploy: %s\n", err)
		return
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	newns, err := netns.New()
	if err != nil {
		fmt.Fprintf(stderr, "envdeploy: network namespace creation failed: %s\n", err)
		return
	}
	defer func() {
		newns.Close()
		netns.Set(bgNetns)
	}()

	hostIf := allocHostIfName()

	runInBackgroundNetns(func() error {
		cmd := exec.Command("ip", "link", "delete", hostIf)
		cmd.Stdout = devNull
		cmd.Stderr = devNull
		cmd.Run()
		return nil
	})

	err = createRouteFromCurrentNetns(hostIf)
	if err != nil {
		fmt.Fprintf(stderr, "envdeploy: failed to create veth interface pair: %s\n", err)
	}

	defer func() {
		runInBackgroundNetns(func() error {
			cmd := exec.Command("ip", "link", "delete", hostIf)
			cmd.Stdout = stderr
			cmd.Stderr = stderr
			cmd.Run()
			return nil
		})
	}()

	hostIp := fmt.Sprintf("10.0.%d.%d", (hostIfCounter/127)%256, (hostIfCounter%127)*2+0)
	guestIp := fmt.Sprintf("10.0.%d.%d", (hostIfCounter/127)%256, (hostIfCounter%127)*2+1)

	err = runInBackgroundNetns(func() error {
		var cmd *exec.Cmd
		var err error

		cmd = exec.Command("ip", "link", "set", "dev", hostIf, "up")
		cmd.Stdout = stderr
		cmd.Stderr = stderr
		err = cmd.Run()
		if err != nil {
			return err
		}
		cmd = exec.Command("ip", "addr", "add", hostIp+"/31", "dev", hostIf)
		cmd.Stdout = stderr
		cmd.Stderr = stderr
		err = cmd.Run()
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "envdeploy: could not set up host veth interface: %s\n", err)
		return
	}

	guestCmds := [][]string{
		{"ip", "link", "set", "dev", "lo", "up"},
		{"ip", "link", "set", "dev", "eth0", "up"},
		{"ip", "addr", "add", guestIp + "/31", "dev", "eth0"},
		{"ip", "route", "add", hostIp, "dev", "eth0"},
		{"ip", "route", "add", "default", "via", hostIp},
	}

	for _, cmds := range guestCmds {
		err = exec.Command(cmds[0], cmds[1:]...).Run()
		if err != nil {
			fmt.Fprintf(stderr, "envdeploy: error executing '%s': %s\n", strings.Join(cmds, " "), err)
			return
		}
	}

	proc, err = startProcessInCgroup(path, argv, env, dir, cgroupPath, stderr)

	if err != nil {
		fmt.Fprintf(stderr, "envdeploy: starting process failed: %s\n", err)
		return
	}

	state, err = proc.Wait()
	if err != nil {
		fmt.Fprintf(stderr, "envdeploy: wait on entry process: %s\n", err)
		return
	}
	fmt.Fprintf(stderr, "envdeploy: entry process exited: %s\n", state)
	done()

	pd.loop()
}

func RunContainedWithDialer(id string, cmd string, env []string, dir string, cgroupDir string, stderr *os.File, pd *pinnedDialer) {
	donech := make(chan interface{})
	go runContainedWithDialerThread(id, cmd, env, dir, cgroupDir, stderr, pd, donech)
	<-donech
}

func Sh(shCmd string) string {
	cmd := exec.Command("/bin/sh", "-c", shCmd)
	cmd.Stdin = devNull
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return fmt.Sprintf("running '%s' failed: %v", shCmd, err.Error())
	}
	return out.String()
}
