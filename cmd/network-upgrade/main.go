// network-upgrade stages and verifies one official relay through an operator-owned SSH alias.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"regexp"
	"syscall"

	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/builtinnetwork"
	"github.com/windshare/windshare/internal/networkprobe"
)

const maintenanceTokenEnv = "FLEET_MAINTENANCE_TOKEN"

var (
	sshAlias    = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._-]*$")
	commitID    = regexp.MustCompile("^[a-f0-9]{40}$")
	operationID = regexp.MustCompile("^[a-f0-9]{32}$")
)

func main() {
	host := flag.String("host", "", "pinned SSH config alias")
	node := flag.String("node-key", "", "enrolled node public key")
	artifact := flag.String("artifact", "", "candidate wsrelay executable")
	revision := flag.String("revision", "", "40-character source commit")
	recoverID := flag.String("recover-operation", "", "verify and release an interrupted operation by its logged ID")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	err := run(*host, *node, *artifact, *revision, *recoverID)
	if err != nil {
		slog.Error("upgrade ended", "error", err)
		if errors.Is(err, errDeferred) {
			os.Exit(busyExit)
		}
		os.Exit(1)
	}
}

func run(host, key, path, revision, recoverID string) error {
	if !sshAlias.MatchString(host) {
		return errors.New("expected a pinned SSH alias")
	}
	if recoverID == "" && (!commitID.MatchString(revision) || path == "") {
		return errors.New("expected a candidate artifact and 40-character revision")
	}
	if recoverID != "" && (!operationID.MatchString(recoverID) || path != "" || revision != "") {
		return errors.New("recovery requires only the host, node key and original operation ID")
	}
	publicKey, err := serviceauthority.PublicKey(key)
	if err != nil || base64.RawURLEncoding.EncodeToString(publicKey[:]) != key {
		return errors.New("expected a canonical enrolled node key")
	}
	token := os.Getenv(maintenanceTokenEnv)
	if len(token) < 32 {
		return errors.New("set FLEET_MAINTENANCE_TOKEN")
	}
	config, err := builtinnetwork.Config("")
	if err != nil {
		return err
	}
	if config.Authority == nil {
		return errors.New("release has no authority")
	}
	var id [16]byte
	rand.Read(id[:])
	op := recoverID
	if op == "" {
		op = hex.EncodeToString(id[:])
	}
	r := runner{op: operation{key, op}, authority: config.Authority.URL, host: host, token: token, api: networkprobe.ReleaseClient(), probe: networkprobe.Client()}
	defer r.api.CloseIdleConnections()
	defer r.probe.CloseIdleConnections()
	log := slog.With("operation_id", r.op.ID, "node_id", key, "revision", revision)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if recoverID != "" {
		log.Info("recovering interrupted maintenance")
		return (rollout{maintenanceTarget: &r}).recover(ctx, log)
	}
	return r.stageAndUpgrade(ctx, log, path, revision)
}

func (r *runner) stageAndUpgrade(ctx context.Context, log *slog.Logger, path, revision string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	log.Info("staging release")
	if err = r.remote(ctx, file, "stage", revision, hex.EncodeToString(hash.Sum(nil))); err != nil {
		return err
	}
	return (rollout{maintenanceTarget: r}).upgrade(ctx, log)
}
