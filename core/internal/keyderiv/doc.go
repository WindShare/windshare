// Package keyderiv owns version-separated HKDF-SHA256 derivation.
//
// V1 retains its historical literal info bytes. Suite 0x02 uses
// ASCII(label)||0x00||context for every branch; keeping the schemes in distinct
// functions prevents a caller from accidentally applying one version's domain
// convention to the other.
package keyderiv
