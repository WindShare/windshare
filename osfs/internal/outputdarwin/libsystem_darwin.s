//go:build darwin && (amd64 || arm64)

#include "textflag.h"

TEXT fgetattrlistTrampoline<>(SB),NOSPLIT,$0-0
	JMP windshare_fgetattrlist(SB)
GLOBL ·fgetattrlistAddress(SB), RODATA, $8
DATA ·fgetattrlistAddress(SB)/8, $fgetattrlistTrampoline<>(SB)

TEXT fsetattrlistTrampoline<>(SB),NOSPLIT,$0-0
	JMP windshare_fsetattrlist(SB)
GLOBL ·fsetattrlistAddress(SB), RODATA, $8
DATA ·fsetattrlistAddress(SB)/8, $fsetattrlistTrampoline<>(SB)
