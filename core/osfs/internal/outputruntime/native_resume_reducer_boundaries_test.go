package outputruntime

import (
	"bytes"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
	"github.com/windshare/windshare/core/transfer"
)

const resumeReducerObjectFill = 0xc7

func TestOrdinaryResumeReducerFailsClosedAcrossIncompleteObservations(t *testing.T) {
	object, err := checkpointmodel.ObjectIDFromBytes(
		bytes.Repeat([]byte{resumeReducerObjectFill}, transfer.OwnedObjectIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	ownedReady, err := fileexecution.NewOwnedObservation(object, fileexecution.OwnedReady)
	if err != nil {
		t.Fatal(err)
	}
	ownedAbsent, err := fileexecution.NewOwnedObservation(object, fileexecution.OwnedAbsent)
	if err != nil {
		t.Fatal(err)
	}
	finalExact, err := fileexecution.ObserveFinal(fileexecution.FinalOwnedExact)
	if err != nil {
		t.Fatal(err)
	}
	finalAbsent, err := fileexecution.ObserveFinal(fileexecution.FinalAbsent)
	if err != nil {
		t.Fatal(err)
	}
	finalCollision, err := fileexecution.ObserveFinal(fileexecution.FinalCollision)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		got  ordinaryResumeDecision
		want ordinaryResumeDecision
	}{
		{
			name: "unknown durable phase",
			got:  reduceOrdinaryResumeRecord(checkpointmodel.Record{}, ownedReady, finalAbsent),
			want: ordinaryResumeBlock(resumeauthority.ItemBlockCheckpointInvalid),
		},
		{
			name: "published exact final",
			got:  reducePublishedResumeRecord(finalExact),
			want: ordinaryResumeState(resumeauthority.ItemPublished),
		},
		{
			name: "publishing exact final",
			got:  reducePublishingResumeRecord(ownedAbsent, finalExact),
			want: ordinaryResumeState(resumeauthority.ItemPublished),
		},
		{
			name: "publishing lost object",
			got:  reducePublishingResumeRecord(ownedAbsent, finalAbsent),
			want: ordinaryResumeBlock(resumeauthority.ItemBlockOwnedObjectUnknown),
		},
		{
			name: "publishing unknown final",
			got:  reducePublishingResumeRecord(ownedReady, fileexecution.FinalObservation{}),
			want: ordinaryResumeBlock(resumeauthority.ItemBlockPublicationUnknown),
		},
		{
			name: "writable collision without object",
			got:  reduceWritableResumeRecord(checkpointmodel.Record{}, ownedAbsent, finalCollision),
			want: ordinaryResumeBlock(resumeauthority.ItemBlockOwnedObjectUnknown),
		},
		{
			name: "writable unknown final",
			got:  reduceWritableResumeRecord(checkpointmodel.Record{}, ownedReady, fileexecution.FinalObservation{}),
			want: ordinaryResumeBlock(resumeauthority.ItemBlockPublicationUnknown),
		},
		{
			name: "writable unknown object",
			got:  reduceAbsentWritableResumeRecord(checkpointmodel.Record{}, fileexecution.OwnedObservation{}),
			want: ordinaryResumeBlock(resumeauthority.ItemBlockOwnedObjectUnknown),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("decision = %+v, want %+v", test.got, test.want)
			}
		})
	}
}
