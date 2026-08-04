package testloopback

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
)

const fixtureOperationTimeout = 5 * time.Second

func TestFixtureOwnsLiveTCPAndUDPSocketsUntilClose(t *testing.T) {
	fixture := New(t)
	tcp := fixture.ListenTCP()
	udp := fixture.ListenUDP()
	if !tcp.Addr().(*net.TCPAddr).IP.Equal(loopbackIPv4) || !udp.LocalAddr().(*net.UDPAddr).IP.Equal(loopbackIPv4) {
		t.Fatalf("fixture exposed non-loopback addresses: TCP=%s UDP=%s", tcp.Addr(), udp.LocalAddr())
	}

	accepted := make(chan error, 1)
	go func() {
		connection, err := tcp.Accept()
		if err == nil {
			_, err = connection.Write([]byte("tcp"))
			err = errors.Join(err, connection.Close())
		}
		accepted <- err
	}()
	client, err := net.DialTimeout("tcp4", tcp.Addr().String(), fixtureOperationTimeout)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := io.ReadAll(client)
	if err = errors.Join(err, client.Close(), <-accepted); err != nil || string(encoded) != "tcp" {
		t.Fatalf("TCP exchange = %q, %v", encoded, err)
	}

	if err := udp.SetReadDeadline(time.Now().Add(fixtureOperationTimeout)); err != nil {
		t.Fatal(err)
	}
	udpClient, err := net.DialUDP("udp4", nil, udp.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := udpClient.Write([]byte("udp")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 8)
	count, _, err := udp.ReadFromUDP(buffer)
	if err = errors.Join(err, udpClient.Close()); err != nil || string(buffer[:count]) != "udp" {
		t.Fatalf("UDP exchange = %q, %v", buffer[:count], err)
	}

	if err := fixture.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Close(); err != nil {
		t.Fatalf("idempotent fixture close: %v", err)
	}
	select {
	case <-tcp.Closed():
	default:
		t.Fatal("TCP owner did not publish closure")
	}
	select {
	case <-udp.Closed():
	default:
		t.Fatal("UDP owner did not publish closure")
	}
	if _, err := udp.WriteTo([]byte("late"), udp.LocalAddr()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed UDP socket write = %v, want net.ErrClosed", err)
	}
}

type recordingCloser struct {
	name  string
	order *[]string
	err   error
}

func (closer recordingCloser) Close() error {
	*closer.order = append(*closer.order, closer.name)
	return closer.err
}

func TestFixtureRetiresInReverseOrderAndJoinsFailures(t *testing.T) {
	firstFailure := errors.New("first failed")
	secondFailure := errors.New("second failed")
	var order []string
	fixture := &Fixture{}
	if err := fixture.own("first", recordingCloser{name: "first", order: &order, err: firstFailure}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.own("second", recordingCloser{name: "second", order: &order, err: secondFailure}); err != nil {
		t.Fatal(err)
	}
	err := fixture.Close()
	if !errors.Is(err, firstFailure) || !errors.Is(err, secondFailure) {
		t.Fatalf("joined cleanup failure = %v", err)
	}
	if got := bytes.Join([][]byte{[]byte(order[0]), []byte(order[1])}, []byte(",")); string(got) != "second,first" {
		t.Fatalf("cleanup order = %v", order)
	}
	if err := fixture.own("late", recordingCloser{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("late owner registration = %v, want ErrClosed", err)
	}
}

type recordingTestContext struct {
	cleanups []func()
	errors   []string
}

func (*recordingTestContext) Helper() {}

func (context *recordingTestContext) Cleanup(cleanup func()) {
	context.cleanups = append(context.cleanups, cleanup)
}

func (context *recordingTestContext) Errorf(format string, values ...any) {
	context.errors = append(context.errors, fmt.Sprintf(format, values...))
}

func (*recordingTestContext) Fatalf(format string, values ...any) {
	panic(fmt.Sprintf(format, values...))
}

func TestFixtureCleanupFailureIsReportedToTheTest(t *testing.T) {
	closeFailure := errors.New("socket close failed")
	context := &recordingTestContext{}
	fixture := New(context)
	if err := fixture.own("failing socket", recordingCloser{order: &[]string{}, err: closeFailure}); err != nil {
		t.Fatal(err)
	}
	for index := range slices.Backward(context.cleanups) {
		context.cleanups[index]()
	}
	if len(context.errors) != 1 || !strings.Contains(context.errors[0], closeFailure.Error()) {
		t.Fatalf("cleanup verdict = %v", context.errors)
	}
}

func TestPionAPIsUseDistinctHeldLoopbackUDPMuxes(t *testing.T) {
	fixture := New(t)
	offererAPI := fixture.NewPionAPI()
	answererAPI := fixture.NewPionAPI()
	if offererAPI.LocalAddr().Port == answererAPI.LocalAddr().Port {
		t.Fatalf("Pion endpoints shared UDP authority: %s", offererAPI.LocalAddr())
	}
	offerer, err := offererAPI.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	answerer, err := answererAPI.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}

	assertedCandidates := make(chan error, 8)
	observe := func(owner *PionAPI) func(*pion.ICECandidate) {
		return func(candidate *pion.ICECandidate) {
			if candidate == nil {
				return
			}
			if candidate.Protocol != pion.ICEProtocolUDP || candidate.Address != loopbackIPv4Address || candidate.Port != uint16(owner.LocalAddr().Port) {
				assertedCandidates <- errors.New("Pion candidate escaped its held UDP loopback owner")
				return
			}
			assertedCandidates <- nil
		}
	}
	offerer.OnICECandidate(observe(offererAPI))
	answerer.OnICECandidate(observe(answererAPI))

	remoteChannel := make(chan *pion.DataChannel, 1)
	answerer.OnDataChannel(func(channel *pion.DataChannel) { remoteChannel <- channel })
	channel, err := offerer.CreateDataChannel("fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	opened := make(chan struct{})
	var openedOnce sync.Once
	channel.OnOpen(func() { openedOnce.Do(func() { close(opened) }) })
	negotiatePionFixturePeers(t, offerer, answerer)

	var remote *pion.DataChannel
	select {
	case remote = <-remoteChannel:
	case <-time.After(fixtureOperationTimeout):
		t.Fatal("answerer did not receive the fixture DataChannel")
	}
	received := make(chan []byte, 1)
	remote.OnMessage(func(message pion.DataChannelMessage) { received <- append([]byte(nil), message.Data...) })
	select {
	case <-opened:
	case <-time.After(fixtureOperationTimeout):
		t.Fatal("fixture DataChannel did not open")
	}
	if err := channel.SendText("loopback"); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-received:
		if string(message) != "loopback" {
			t.Fatalf("Pion payload = %q", message)
		}
	case <-time.After(fixtureOperationTimeout):
		t.Fatal("fixture DataChannel did not deliver its payload")
	}
	for range 2 {
		select {
		case candidateErr := <-assertedCandidates:
			if candidateErr != nil {
				t.Fatal(candidateErr)
			}
		case <-time.After(fixtureOperationTimeout):
			t.Fatal("loopback Pion candidate was not observed")
		}
	}

	if err := fixture.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := offererAPI.NewPeerConnection(pion.Configuration{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Pion API creation = %v, want ErrClosed", err)
	}
}

func negotiatePionFixturePeers(t *testing.T, offerer, answerer *pion.PeerConnection) {
	t.Helper()
	offer, err := offerer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	offerGathered := pion.GatheringCompletePromise(offerer)
	if err := offerer.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	waitPionGathering(t, offerGathered)
	if err := answerer.SetRemoteDescription(*offerer.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	answer, err := answerer.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	answerGathered := pion.GatheringCompletePromise(answerer)
	if err := answerer.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	waitPionGathering(t, answerGathered)
	if err := offerer.SetRemoteDescription(*answerer.LocalDescription()); err != nil {
		t.Fatal(err)
	}
}

func waitPionGathering(t *testing.T, complete <-chan struct{}) {
	t.Helper()
	select {
	case <-complete:
	case <-time.After(fixtureOperationTimeout):
		t.Fatal("Pion candidate gathering did not complete")
	}
}
