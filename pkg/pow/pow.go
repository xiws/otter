// Package pow 实现 DeepSeek 的 DeepSeekHashV1 工作量证明（PoW）求解。
//
// DeepSeek 在对话、文件上传等接口前加了 PoW 校验：服务端下发 challenge，
// 客户端必须搜索出一个 answer 才能继续。求解逻辑由 DeepSeek 前端的
// WebAssembly 模块完成——该模块内部是内联的 SHA3-256 与 Keccak-f 置换，
// 算法细节未公开，因此这里直接嵌入并调用官方模块，而不是重新实现哈希。
//
// 嵌入的 sha3_wasm.wasm 即 DeepSeek 前端分发的模块：
//
//	SHA256 b3fca8cc072c1defbd60c02266a8e48bd307a1804aaff4314900aea720e72f7d
//	size   26612 bytes
//
// 它与 chat.deepseek.com 前端加载的 sha3_wasm_bg.*.wasm 为同一文件。
package pow

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"

	"github.com/tetratelabs/wazero"
)

//go:embed sha3_wasm.wasm
var wasmBytes []byte

// SupportedAlgorithm 是目前唯一已知的 PoW 算法标识。
const SupportedAlgorithm = "DeepSeekHashV1"

// Challenge 表示服务端下发的一次 PoW 挑战。
type Challenge struct {
	Algorithm  string  `json:"algorithm"`
	Challenge  string  `json:"challenge"`
	Salt       string  `json:"salt"`
	Signature  string  `json:"signature"`
	Difficulty float64 `json:"difficulty"`
	ExpireAt   int64   `json:"expire_at"`
	TargetPath string  `json:"target_path"`
}

// Solve 求解挑战，返回 answer。
//
// 每次调用都会新建一个独立的 WASM 实例：模块内存无法跨请求复用，
// 而单次求解（难度 144000 时约十余毫秒）的开销可以接受。
func (c *Challenge) Solve(ctx context.Context) (uint64, error) {
	if c.Algorithm != SupportedAlgorithm {
		return 0, fmt.Errorf("不支持的 PoW 算法: %s", c.Algorithm)
	}

	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)

	mod, err := rt.Instantiate(ctx, wasmBytes)
	if err != nil {
		return 0, fmt.Errorf("加载 PoW 模块失败: %w", err)
	}
	defer mod.Close(ctx)

	alloc := mod.ExportedFunction("__wbindgen_export_0")
	solve := mod.ExportedFunction("wasm_solve")
	stackAdd := mod.ExportedFunction("__wbindgen_add_to_stack_pointer")
	mem := mod.Memory()
	if alloc == nil || solve == nil || stackAdd == nil || mem == nil {
		return 0, fmt.Errorf("PoW 模块缺少必要的导出项")
	}

	// writeString 把字符串写入模块线性内存，返回 (指针, 字节长度)。
	writeString := func(s string) (uint32, uint32, error) {
		b := []byte(s)
		// wasm-bindgen 分配器签名: alloc(size, align) -> pointer
		res, err := alloc.Call(ctx, uint64(len(b)), 1)
		if err != nil {
			return 0, 0, fmt.Errorf("PoW 模块内存分配失败: %w", err)
		}
		p := uint32(res[0])
		if len(b) > 0 && !mem.Write(p, b) {
			return 0, 0, fmt.Errorf("写入 PoW 模块内存失败")
		}
		return p, uint32(len(b)), nil
	}

	challengePtr, challengeLen, err := writeString(c.Challenge)
	if err != nil {
		return 0, err
	}
	// 前端拼接规则："{salt}_{expire_at}_"，answer 由模块内部追加。
	prefix := fmt.Sprintf("%s_%d_", c.Salt, c.ExpireAt)
	prefixPtr, prefixLen, err := writeString(prefix)
	if err != nil {
		return 0, err
	}

	// wasm-bindgen 约定：先在栈上预留 16 字节返回区，
	// +0 为 i32 状态（0 表示无解），+8 为 f64 答案。
	// i32 参数 -16 以低 32 位传入。
	retPtr, err := stackAdd.Call(ctx, uint64(uint32(0xFFFFFFF0)))
	if err != nil {
		return 0, fmt.Errorf("PoW 模块栈分配失败: %w", err)
	}
	defer func() { _, _ = stackAdd.Call(ctx, 16) }()

	if _, err := solve.Call(ctx,
		uint64(uint32(retPtr[0])),
		uint64(challengePtr), uint64(challengeLen),
		uint64(prefixPtr), uint64(prefixLen),
		math.Float64bits(c.Difficulty),
	); err != nil {
		return 0, fmt.Errorf("PoW 求解失败: %w", err)
	}

	addr := uint32(retPtr[0])
	status, ok := mem.ReadUint32Le(addr)
	if !ok {
		return 0, fmt.Errorf("读取 PoW 状态失败")
	}
	if status == 0 {
		return 0, fmt.Errorf("PoW 未找到解（难度 %v，挑战 %s）", c.Difficulty, c.Challenge)
	}

	raw, ok := mem.Read(addr+8, 8)
	if !ok {
		return 0, fmt.Errorf("读取 PoW 答案失败")
	}
	answer := math.Float64frombits(binary.LittleEndian.Uint64(raw))
	if math.IsNaN(answer) || math.IsInf(answer, 0) || answer < 0 {
		return 0, fmt.Errorf("PoW 返回了无效答案: %v", answer)
	}
	return uint64(answer), nil
}

// HeaderValue 把挑战与答案编码成 x-ds-pow-response 请求头的值。
// 字段顺序与服务端前端保持一致。
func (c *Challenge) HeaderValue(answer uint64) (string, error) {
	payload := struct {
		Algorithm  string `json:"algorithm"`
		Challenge  string `json:"challenge"`
		Salt       string `json:"salt"`
		Answer     uint64 `json:"answer"`
		Signature  string `json:"signature"`
		TargetPath string `json:"target_path"`
	}{
		Algorithm:  c.Algorithm,
		Challenge:  c.Challenge,
		Salt:       c.Salt,
		Answer:     answer,
		Signature:  c.Signature,
		TargetPath: c.TargetPath,
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("编码 PoW 响应失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// SolveHeader 求解挑战并直接返回请求头的值。
func (c *Challenge) SolveHeader(ctx context.Context) (string, error) {
	answer, err := c.Solve(ctx)
	if err != nil {
		return "", err
	}
	return c.HeaderValue(answer)
}
