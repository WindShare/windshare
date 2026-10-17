package v2

import (
	"errors"
	"testing"
)

func TestDeliveryControlsSharedVectors(t *testing.T) {
	for _, codec := range []struct {
		name    string
		marshal func(deliveryTotals) ([]byte, error)
		parse   func([]byte) (deliveryTotals, error)
	}{
		{
			"relay-delivery-consumed",
			func(f deliveryTotals) ([]byte, error) { return DeliveryConsumed(f).MarshalBinary() },
			func(b []byte) (deliveryTotals, error) {
				f, err := ParseDeliveryConsumed(b)
				return deliveryTotals(f), err
			},
		},
		{
			"relay-settlement-notice",
			func(f deliveryTotals) ([]byte, error) { return SettlementNotice(f).MarshalBinary() },
			func(b []byte) (deliveryTotals, error) {
				f, err := ParseSettlementNotice(b)
				return deliveryTotals(f), err
			},
		},
		{
			"relay-settlement-ack",
			func(f deliveryTotals) ([]byte, error) { return SettlementAck(f).MarshalBinary() },
			func(b []byte) (deliveryTotals, error) {
				f, err := ParseSettlementAck(b)
				return deliveryTotals(f), err
			},
		},
	} {
		t.Run(codec.name, func(t *testing.T) {
			var vector struct {
				RelaySessionIDB64 string `json:"relaySessionIdB64"`
				Counts            []struct {
					Name              string `json:"name"`
					TotalFrames       string `json:"totalFrames"`
					TotalEncodedBytes string `json:"totalEncodedBytes"`
					EncodedB64        string `json:"encodedB64"`
					Accepted          bool   `json:"accepted"`
				} `json:"counts"`
				Malformed []struct {
					Name       string `json:"name"`
					EncodedB64 string `json:"encodedB64"`
				} `json:"malformed"`
			}
			loadV2VectorCase(t, "v2-session.json", codec.name, &vector)
			var session RelaySessionID
			copy(session[:], testB64(t, vector.RelaySessionIDB64))
			for _, test := range vector.Counts {
				t.Run(test.Name, func(t *testing.T) {
					control := deliveryTotals{
						RelaySessionID:    session,
						TotalFrames:       parseVectorUint64(t, test.TotalFrames),
						TotalEncodedBytes: parseVectorUint64(t, test.TotalEncodedBytes),
					}
					wire := testB64(t, test.EncodedB64)
					decoded, decodeErr := codec.parse(wire)
					if !test.Accepted {
						_, encodeErr := codec.marshal(control)
						if !errors.Is(decodeErr, ErrMalformed) || !errors.Is(encodeErr, ErrMalformed) {
							t.Fatalf("invalid totals: decode=%v encode=%v", decodeErr, encodeErr)
						}
						return
					}
					assertBinaryVector(t, codec.name, func() ([]byte, error) { return codec.marshal(control) }, test.EncodedB64)
					if decodeErr != nil || decoded != control {
						t.Fatalf("decode=%+v error=%v want=%+v", decoded, decodeErr, control)
					}
					if _, err := ParseReceiveCredit(wire); err == nil {
						t.Fatal("current runtime must not reinterpret cumulative totals as a delta")
					}
				})
			}
			for _, test := range vector.Malformed {
				t.Run(test.Name, func(t *testing.T) {
					if _, err := codec.parse(testB64(t, test.EncodedB64)); err == nil {
						t.Fatal("accepted malformed control")
					}
				})
			}
			if _, err := codec.marshal(deliveryTotals{}); !errors.Is(err, ErrIdentity) {
				t.Fatalf("zero session ID: %v", err)
			}
		})
	}
}
