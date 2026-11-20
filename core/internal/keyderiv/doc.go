// Package keyderiv owns suite-0x02 HKDF-SHA256 derivation.
//
// Every branch uses ASCII(label)||0x00||context with an empty salt so Go and
// TypeScript implementations align on a single info parameter.
package keyderiv
