// WindShare's only native ABI is host lifetime and C-owned result buffers.
// Commands and observations use the authenticated loopback WebSocket.
package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"sync"
	"unsafe"

	"github.com/windshare/windshare/host/desktop"
)

const abiVersion = 1

var registry struct {
	sync.Mutex
	next   uint64
	host   *desktop.Host
	handle uint64
}

type response struct {
	ABIVersion int               `json:"abi_version"`
	Handle     uint64            `json:"handle,omitempty"`
	Endpoint   *desktop.Endpoint `json:"endpoint,omitempty"`
	Error      string            `json:"error,omitempty"`
}

func startHost() response {
	registry.Lock()
	defer registry.Unlock()
	result := response{ABIVersion: abiVersion}
	if registry.host != nil {
		result.Error = "the application host is already started"
		return result
	}
	host, err := desktop.Start()
	if err != nil {
		result.Error = err.Error()
		return result
	}
	registry.next++
	registry.host, registry.handle = host, registry.next
	result.Handle, result.Endpoint = registry.handle, &host.Endpoint
	return result
}

func stopHost(handle uint64) response {
	registry.Lock()
	defer registry.Unlock()
	result := response{ABIVersion: abiVersion}
	if registry.host == nil || registry.handle != handle {
		result.Error = "unknown host handle"
		return result
	}
	if err := registry.host.Close(); err != nil {
		result.Error = err.Error()
	}
	registry.host = nil
	return result
}

func nativeResult(result response) *C.char {
	data, err := json.Marshal(result)
	if err != nil {
		data = []byte(`{"abi_version":1,"error":"host response encoding failed"}`)
	}
	return C.CString(string(data))
}

//export WindShareHostStart
func WindShareHostStart() *C.char { return nativeResult(startHost()) }

//export WindShareHostStop
func WindShareHostStop(handle C.ulonglong) *C.char { return nativeResult(stopHost(uint64(handle))) }

//export WindShareHostFree
func WindShareHostFree(value *C.char) { C.free(unsafe.Pointer(value)) }

func main() {}
