package gateway

// SOAP argument order follows the UPnP service action schema; alphabetical
// maps work with permissive devices but fail on conforming ordered decoders.
func soapArgumentOrder(action string) []string {
	switch action {
	case "AddPortMapping", "AddAnyPortMapping":
		return []string{"NewRemoteHost", "NewExternalPort", "NewProtocol", "NewInternalPort", "NewInternalClient", "NewEnabled", "NewPortMappingDescription", "NewLeaseDuration"}
	case "GetSpecificPortMappingEntry", "DeletePortMapping":
		return []string{"NewRemoteHost", "NewExternalPort", "NewProtocol"}
	case "AddPinhole":
		return []string{"RemoteHost", "RemotePort", "InternalClient", "InternalPort", "Protocol", "LeaseTime"}
	case "UpdatePinhole":
		return []string{"UniqueID", "NewLeaseTime"}
	case "DeletePinhole":
		return []string{"UniqueID"}
	default:
		return nil
	}
}
