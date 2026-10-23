package transfer

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

func TestLaneSetCloseJoinsBlockedHedgeAfterFastWinner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		descriptor := transferDescriptor(t, 1)
		demand := validDemand(t, descriptor, 0)
		winnerRecord := transferRecord(t, descriptor, 0)
		winnerStarted := make(chan struct{})
		loserStarted := make(chan struct{})
		allowWinner := make(chan struct{})
		loserCancelled := make(chan struct{})
		allowLoserExit := make(chan struct{})

		lanes, err := transferlanes.New(transferlanes.Config{
			ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](91),
			RaceWidth:         2,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := lanes.Add(transferlanes.Identity{ID: 1}, transferlanes.RouteRelay, laneFunction(func(context.Context, transferlanes.Demand) (records.BlockRecord, error) {
			close(winnerStarted)
			<-allowWinner
			return winnerRecord, nil
		})); err != nil {
			t.Fatal(err)
		}
		if err := lanes.Add(transferlanes.Identity{ID: 2}, transferlanes.RouteRelay, laneFunction(func(ctx context.Context, _ transferlanes.Demand) (records.BlockRecord, error) {
			close(loserStarted)
			<-ctx.Done()
			close(loserCancelled)
			<-allowLoserExit
			return records.BlockRecord{}, ctx.Err()
		})); err != nil {
			t.Fatal(err)
		}
		suspendedIdentity := transferlanes.Identity{ID: 3}
		if err := lanes.Add(suspendedIdentity, transferlanes.RouteRelay, laneFunction(func(ctx context.Context, _ transferlanes.Demand) (records.BlockRecord, error) {
			<-ctx.Done()
			return records.BlockRecord{}, ctx.Err()
		})); err != nil {
			t.Fatal(err)
		}
		if _, err := lanes.SuspendContent(suspendedIdentity); err != nil {
			t.Fatal(err)
		}

		process, err := NewPlaintextBudget(uint64(catalog.MinChunkSize) * 4)
		if err != nil {
			t.Fatal(err)
		}
		broker, err := NewBlockBroker(BlockBrokerConfig{
			ShareInstance: descriptor.ShareInstance(),
			Blocks:        lanes,
			MaxBytes:      uint64(catalog.MinChunkSize) * 4,
			ProcessBudget: process,
		})
		if err != nil {
			t.Fatal(err)
		}
		lease := transferID[content.LeaseID](12)
		fetchDone := make(chan error, 1)
		go func() {
			_, fetchErr := broker.GetBlock(context.Background(), lease, descriptor, 0)
			fetchDone <- fetchErr
		}()
		<-winnerStarted
		<-loserStarted
		close(allowWinner)
		if err := <-fetchDone; err != nil {
			t.Fatalf("fast winner failed: %v", err)
		}
		<-loserCancelled
		brokerCloseDone := make(chan struct{})
		go func() {
			broker.Close()
			close(brokerCloseDone)
		}()
		select {
		case <-brokerCloseDone:
		case <-time.After(time.Second):
			close(allowLoserExit)
			t.Fatal("broker close tried to join a Set-owned hedge")
		}
		synctest.Wait()
		if broker.UsedBytes() != uint64(catalog.MinChunkSize) || process.Used() != uint64(catalog.MinChunkSize) {
			t.Fatalf("pending hedge lost ownership: broker=%d process=%d", broker.UsedBytes(), process.Used())
		}

		firstCloseDone := make(chan struct{})
		secondCloseDone := make(chan struct{})
		go func() {
			lanes.Close()
			close(firstCloseDone)
		}()
		go func() {
			lanes.Close()
			close(secondCloseDone)
		}()
		synctest.Wait()
		select {
		case <-firstCloseDone:
			t.Fatal("lane close returned while its cancelled hedge was still running")
		default:
		}
		select {
		case <-secondCloseDone:
			t.Fatal("concurrent lane close returned before the shared join completed")
		default:
		}

		close(allowLoserExit)
		<-firstCloseDone
		<-secondCloseDone

		if lanes.Len() != 0 || broker.UsedBytes() != 0 || process.Used() != 0 {
			t.Fatalf("closed ownership survived: lanes=%d broker=%d process=%d", lanes.Len(), broker.UsedBytes(), process.Used())
		}
		if _, err := lanes.Fetch(context.Background(), transferlanes.Request{Demand: demand, Validate: validateTransferRecord(demand)}); !errors.Is(err, transferlanes.ErrClosed) {
			t.Fatalf("closed lane set admitted a demand: %v", err)
		}
	})
}
