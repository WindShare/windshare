//go:build darwin && (amd64 || arm64)

#include "textflag.h"

TEXT futimensTrampoline<>(SB),NOSPLIT,$0-0
	JMP windshare_output_futimens(SB)
GLOBL ·futimensAddress(SB), RODATA, $8
DATA ·futimensAddress(SB)/8, $futimensTrampoline<>(SB)

TEXT fsetattrlistTrampoline<>(SB),NOSPLIT,$0-0
	JMP windshare_output_fsetattrlist(SB)
GLOBL ·fsetattrlistAddress(SB), RODATA, $8
DATA ·fsetattrlistAddress(SB)/8, $fsetattrlistTrampoline<>(SB)
