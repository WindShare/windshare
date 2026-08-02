package liveshare

import (
	"fmt"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestReadyPathDoesNotEnumerateDescendants(t *testing.T) {
	var baselineRegistrationMaterial uint64
	for scaleIndex, descendants := range readyDescendantScales {
		t.Run(fmt.Sprintf("descendants=%07d", descendants), func(t *testing.T) {
			measurement, closeReady, err := prepareVirtualReady(descendants)
			if err != nil {
				t.Fatal(err)
			}
			if err := closeReady(); err != nil {
				t.Fatal(err)
			}
			if measurement.descendantFSOps != 0 {
				t.Fatalf("ready path performed %d descendant filesystem operations", measurement.descendantFSOps)
			}
			if measurement.descriptorBytes == 0 || measurement.descriptorBytes > catalog.MaxDescriptorObjectBytes {
				t.Fatalf("descriptor budget = %d", measurement.descriptorBytes)
			}

			if scaleIndex == 0 {
				baselineRegistrationMaterial = measurement.registrationMaterialBytes
			}
			if measurement.registrationMaterialBytes != baselineRegistrationMaterial {
				t.Fatalf(
					"registration material grew with virtual descendants: got %d, baseline %d",
					measurement.registrationMaterialBytes,
					baselineRegistrationMaterial,
				)
			}
		})
	}
}
