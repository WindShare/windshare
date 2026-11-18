package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
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
	configuration, err := json.Marshal(map[string]any{
		"listen": address, "database": database, "identity_file": database + ".identity.json",
		"stun": map[string]any{"udp_addresses": []string{"127.0.0.1:0", "[::1]:0"}, "admin_address": "127.0.0.1:0"},
		"node": map[string]any{"services": map[string]bool{"relay": true, "stun": true}, "relay_budget": nil},
	})
	check(err)
	check(os.WriteFile(database+".config.json", configuration, 0o600))
	process := &child{command: exec.Command(binary, "--config", database+".config.json")}
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
		for scanner.Scan() {
			var event struct{ Event string }
			if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Event == "listening" {
				ready <- bytes.Clone(scanner.Bytes())
				return
			}
		}
		ready <- nil
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
