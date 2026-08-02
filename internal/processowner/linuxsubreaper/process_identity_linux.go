//go:build linux

package linuxsubreaper

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type processIdentity struct {
	PID            int
	PPID           int
	StartTimeTicks uint64
	State          byte
}

func directChildInventory(ownerPID int) ([]processIdentity, error) {
	processes, err := readProcessTable()
	if err != nil {
		return nil, err
	}
	inventory := make([]processIdentity, 0)
	for _, candidate := range processes {
		if candidate.PPID == ownerPID {
			inventory = append(inventory, candidate)
		}
	}
	sort.Slice(inventory, func(i, j int) bool {
		if inventory[i].PID != inventory[j].PID {
			return inventory[i].PID < inventory[j].PID
		}
		return inventory[i].StartTimeTicks < inventory[j].StartTimeTicks
	})
	return inventory, nil
}

func readProcessTable() (map[int]processIdentity, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("enumerate procfs: %w", err)
	}
	result := make(map[int]processIdentity)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid < 1 {
			continue
		}
		identity, err := readProcessIdentity(pid)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			// Same-UID descendants remain readable on supported Linux runners.
			// Unrelated hidepid/permission boundaries must not make their process
			// churn an authority over this invocation's authenticated pidfds.
			if errors.Is(err, os.ErrPermission) {
				continue
			}
			return nil, err
		}
		result[pid] = identity
	}
	return result, nil
}

func readStableProcessIdentity(pid int) (processIdentity, error) {
	first, err := readProcessIdentity(pid)
	if err != nil {
		return processIdentity{}, err
	}
	second, err := readProcessIdentity(pid)
	if err != nil {
		return processIdentity{}, err
	}
	if first.PID != second.PID || first.StartTimeTicks != second.StartTimeTicks {
		return processIdentity{}, fmt.Errorf("procfs identity for pid %d changed while read", pid)
	}
	return first, nil
}

func readProcessIdentity(pid int) (processIdentity, error) {
	encoded, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return processIdentity{}, err
	}
	closing := bytes.LastIndex(encoded, []byte(") "))
	if closing < 1 {
		return processIdentity{}, fmt.Errorf("procfs stat for pid %d is malformed", pid)
	}
	fields := strings.Fields(string(encoded[closing+2:]))
	if len(fields) < 20 || len(fields[0]) != 1 {
		return processIdentity{}, fmt.Errorf("procfs stat for pid %d lacks identity fields", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return processIdentity{}, fmt.Errorf("parse procfs parent pid for %d: %w", pid, err)
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return processIdentity{}, fmt.Errorf("parse procfs starttime for %d: %w", pid, err)
	}
	return processIdentity{PID: pid, PPID: ppid, StartTimeTicks: start, State: fields[0][0]}, nil
}

func identityKey(identity processIdentity) string {
	return strconv.Itoa(identity.PID) + "/" + strconv.FormatUint(identity.StartTimeTicks, 10)
}
