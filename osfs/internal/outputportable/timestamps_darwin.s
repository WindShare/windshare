//go:build darwin && (amd64 || arm64)

#include "textflag.h"

TEXT futimensTrampoline<>(SB),NOSPLIT,$0-0
	JMP windshare_output_futimens(SB)
GLOBL ·futimensAddress(SB), RODATA, $8
DATA ·futimensAddress(SB)/8, $futimensTrampoline<>(SB)
