package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// main's tick loop cannot be run without a server, so this pins its SIGUSR2
// arm by syntax: the arm that receives from withdrawSig must call
// withdrawOnSignal, or the operator's withdrawal is never made.
func TestMain_WithdrawSignalArmCallsWithdrawOnSignal(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	arms, called := 0, 0
	ast.Inspect(file, func(n ast.Node) bool {
		cc, ok := n.(*ast.CommClause)
		if !ok || cc.Comm == nil {
			return true
		}
		exprStmt, ok := cc.Comm.(*ast.ExprStmt)
		if !ok {
			return true
		}
		recv, ok := exprStmt.X.(*ast.UnaryExpr)
		if !ok || recv.Op != token.ARROW {
			return true
		}
		if id, ok := recv.X.(*ast.Ident); !ok || id.Name != "withdrawSig" {
			return true
		}
		arms++
		for _, stmt := range cc.Body {
			if es, ok := stmt.(*ast.ExprStmt); ok {
				if call, ok := es.X.(*ast.CallExpr); ok {
					if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "withdrawOnSignal" {
						called++
					}
				}
			}
		}
		return true
	})
	if arms != 1 || called != 1 {
		t.Errorf("%d withdrawSig arms, %d of them calling withdrawOnSignal; want 1 and 1", arms, called)
	}
}
