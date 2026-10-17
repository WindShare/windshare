package v2

import (
	"errors"
	"testing"
)

func TestDeliveryConsumedSharedVectors(t *testing.T) {
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
	loadV2VectorCase(t, "v2-session.json", "relay-delivery-consumed", &vector)
	var session RelaySessionID
	copy(session[:], testB64(t, vector.RelaySessionIDB64))
	for _, test := range vector.Counts {
		t.Run(test.Name, func(t *testing.T) {
			feedback := DeliveryConsumed{
				RelaySessionID:    session,
				TotalFrames:       parseVectorUint64(t, test.TotalFrames),
				TotalEncodedBytes: parseVectorUint64(t, test.TotalEncodedBytes),
			}
			wire := testB64(t, test.EncodedB64)
			decoded, decodeErr := ParseDeliveryConsumed(wire)
			if !test.Accepted {
				_, encodeErr := feedback.MarshalBinary()
				if !errors.Is(decodeErr, ErrMalformed) || !errors.Is(encodeErr, ErrMalformed) {
					t.Fatalf("invalid totals: decode=%v encode=%v", decodeErr, encodeErr)
				}
				return
			}
			assertBinaryVector(t, "DELIVERY_CONSUMED", feedback.MarshalBinary, test.EncodedB64)
			if decodeErr != nil || decoded != feedback {
				t.Fatalf("decode=%+v error=%v want=%+v", decoded, decodeErr, feedback)
			}
			if _, err := ParseReceiveCredit(wire); err == nil {
				t.Fatal("current runtime must not reinterpret cumulative feedback as a delta")
			}
		})
	}
	for _, test := range vector.Malformed {
		t.Run(test.Name, func(t *testing.T) {
			if _, err := ParseDeliveryConsumed(testB64(t, test.EncodedB64)); err == nil {
				t.Fatal("accepted malformed feedback")
			}
		})
	}
}

func TestDeliveryConsumedRequiresSessionIdentity(t *testing.T) {
	if _, err := (DeliveryConsumed{}).MarshalBinary(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("zero session ID: %v", err)
	}
}
