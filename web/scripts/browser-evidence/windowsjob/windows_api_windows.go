//go:build windows

package main

import "golang.org/x/sys/windows"

// Job-information calls intentionally use LazyProc.Call: its uintptrescapes
// contract prevents race instrumentation or stack growth from invalidating the
// structure pointers that x/sys otherwise exposes through a uintptr wrapper.
var (
	kernel32DLL                   = windows.NewLazySystemDLL("kernel32.dll")
	isProcessInJobProcedure       = kernel32DLL.NewProc("IsProcessInJob")
	compareStringOrdinalProcedure = kernel32DLL.NewProc("CompareStringOrdinal")
	getHandleInformationProcedure = kernel32DLL.NewProc("GetHandleInformation")
	setJobInformationProcedure    = kernel32DLL.NewProc("SetInformationJobObject")
	queryJobInformationProcedure  = kernel32DLL.NewProc("QueryInformationJobObject")
)
