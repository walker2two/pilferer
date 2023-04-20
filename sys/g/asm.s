#include "textflag.h"
#include "go_asm.h"
#include "../../src/runtime/go_tls.h"

#define g_m 48 // it's not defined in go_asm when not using runtime package 

TEXT ·GetG(SB),NOSPLIT,$0-8
	get_tls(CX)
	MOVQ	g(CX), AX
	MOVQ	AX, gp+0(FP)
	RET

TEXT ·GetM(SB),NOSPLIT,$0-8
    get_tls(CX)
    MOVQ    g(CX), AX
    MOVQ    g_m(AX), BX
    MOVQ    BX, ret+0(FP)
    RET

