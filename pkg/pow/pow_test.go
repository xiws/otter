package pow

import (
	"context"
	"testing"
	"time"
)

// 真实服务端下发的一组挑战参数（已过期，仅用于本地求解验证）。
const (
	testChallenge = "f3a36528962cec499306687fe2c72329773e0dae744c3ce1d37e7fdd76b12fac"
	testSalt      = "727bc06940c12da0c2ff"
	testExpireAt  = int64(1789201736341)
	testSignature = "90186f45d10d9a01432a34447a7fcaee7c7d175844784edb705968412a8f895f"
)

func newTestChallenge() *Challenge {
	return &Challenge{
		Algorithm:  SupportedAlgorithm,
		Challenge:  testChallenge,
		Salt:       testSalt,
		Signature:  testSignature,
		Difficulty: 144000,
		ExpireAt:   testExpireAt,
		TargetPath: "/api/v0/chat/completion",
	}
}

func TestSolve(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got, err := newTestChallenge().Solve(ctx)
	if err != nil {
		t.Fatalf("Solve 返回错误: %v", err)
	}
	// 求解是确定性的：同一组挑战参数总是得到同一个答案。
	if got != 37551 {
		t.Errorf("Solve = %d, 期望 37551", got)
	}
}

func TestSolveHeader(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h, err := newTestChallenge().SolveHeader(ctx)
	if err != nil {
		t.Fatalf("SolveHeader 返回错误: %v", err)
	}
	if h == "" {
		t.Fatal("SolveHeader 返回空值")
	}
}

func TestSolveRejectsUnknownAlgorithm(t *testing.T) {
	c := newTestChallenge()
	c.Algorithm = "SomethingElse"
	if _, err := c.Solve(context.Background()); err == nil {
		t.Fatal("未知算法应当报错")
	}
}

// 求解难度上限之内必然有解，上限之下的边界应当无解。
func TestSolveDifficultyBoundary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c := newTestChallenge()
	c.Difficulty = 37551 // 答案 37551 恰好在搜索范围之外
	if _, err := c.Solve(ctx); err == nil {
		t.Error("难度 37551 时不应找到解")
	}

	c.Difficulty = 37552 // 刚好覆盖答案
	got, err := c.Solve(ctx)
	if err != nil {
		t.Fatalf("难度 37552 时应当找到解: %v", err)
	}
	if got != 37551 {
		t.Errorf("Solve = %d, 期望 37551", got)
	}
}
