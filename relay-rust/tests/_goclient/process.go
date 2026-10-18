package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

type child struct {
	command  *exec.Cmd
	stderr   bytes.Buffer
	address  string
	endpoint string
}

func start(binary, database, address string) *child {
	process := &child{command: exec.Command(binary, "--database", database, "--listen", address,
		"--stun-udp", "127.0.0.1:0,[::1]:0", "--stun-admin", "127.0.0.1:0")}
	stdout, err := process.command.StdoutPipe()
	check(err)
	process.command.Stderr = &process.stderr
	check(process.command.Start())
	startupComplete := false
	defer func() {
		if !startupComplete {
			process.kill()
		}
	}()
	ready := make(chan []byte, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			ready <- bytes.Clone(scanner.Bytes())
		} else {
			ready <- nil
		}
	}()
	select {
	case bytes := <-ready:
		var state struct {
			Event, Address, Endpoint string
			STUNUDP                  []string `json:"stun_udp"`
			STUNAdmin                string   `json:"stun_admin"`
		}
		err = json.Unmarshal(bytes, &state)
		if err != nil || state.Event != "listening" {
			process.kill()
			panic(fmt.Sprintf("native host startup: %v; %s", err, process.stderr.String()))
		}
		process.address, process.endpoint = state.Address, state.Endpoint
		verifySTUN(state.STUNUDP, state.STUNAdmin)
	case <-time.After(10 * time.Second):
		process.kill()
		panic("native host startup timed out: " + process.stderr.String())
	}
	startupComplete = true
	return process
}

func (process *child) kill() {
	if process.command == nil {
		return
	}
	_ = process.command.Process.Kill()
	_ = process.command.Wait()
	process.command = nil
}
