package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// 阿里云百炼平台配置 (从环境变量获取)
var (
	QwenAppID  = os.Getenv("QWEN_APP_ID")
	QwenAPIKey = os.Getenv("QWEN_API_KEY")
)

// ============== 测试用例 ==============

// TestQwenBasicChat 测试基本同步对话
func TestQwenBasicChat(t *testing.T) {
	requireLiveTests(t)
	agent := NewQwenAgent(QwenAppID, QwenAPIKey)
	ctx := context.Background()

	prompt := "你好，请用一句话介绍你自己"
	t.Logf("用户: %s", prompt)

	start := time.Now()
	resp, err := agent.Chat(ctx, prompt)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	if resp.Output.Text == "" {
		t.Fatal("Empty response text")
	}

	t.Logf("助手: %s", resp.Output.Text)
	t.Logf("耗时: %v, Token: %d", elapsed, resp.Usage.TotalTokens)
}

// TestQwenStreamChat 测试流式输出
func TestQwenStreamChat(t *testing.T) {
	requireLiveTests(t)
	agent := NewQwenAgent(QwenAppID, QwenAPIKey)
	ctx := context.Background()

	prompt := "请用3句话解释什么是量化交易"
	t.Logf("用户: %s", prompt)

	var fullText strings.Builder
	start := time.Now()

	err := agent.ChatStream(ctx, prompt, func(chunk string) {
		fullText.WriteString(chunk)
	})

	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("ChatStream failed: %v", err)
	}

	if fullText.Len() == 0 {
		t.Fatal("Empty stream response")
	}

	t.Logf("助手: %s", fullText.String())
	t.Logf("耗时: %v, 字符数: %d", elapsed, fullText.Len())
}

// TestQwenMultiTurn 测试多轮对话（上下文记忆）
func TestQwenMultiTurn(t *testing.T) {
	requireLiveTests(t)
	agent := NewQwenAgent(QwenAppID, QwenAPIKey)
	ctx := context.Background()

	// 第一轮：设置上下文
	resp1, err := agent.Chat(ctx, "我叫小明，我是一名 Go 程序员，请记住这些信息")
	if err != nil {
		t.Fatalf("Round 1 failed: %v", err)
	}
	t.Logf("[Round 1] 用户: 我叫小明，我是一名 Go 程序员")
	t.Logf("[Round 1] 助手: %s", resp1.Output.Text)
	t.Logf("[Round 1] SessionID: %s", agent.SessionID)

	// 第二轮：验证记忆
	resp2, err := agent.Chat(ctx, "请问我叫什么名字？我是做什么的？")
	if err != nil {
		t.Fatalf("Round 2 failed: %v", err)
	}
	t.Logf("[Round 2] 用户: 请问我叫什么名字？我是做什么的？")
	t.Logf("[Round 2] 助手: %s", resp2.Output.Text)

	// 检查是否记住了信息
	text := strings.ToLower(resp2.Output.Text)
	if !strings.Contains(text, "小明") && !strings.Contains(text, "go") {
		t.Logf("警告: 模型可能没有正确记住上下文")
	}
}

// TestQwenResetSession 测试重置会话
func TestQwenResetSession(t *testing.T) {
	requireLiveTests(t)
	agent := NewQwenAgent(QwenAppID, QwenAPIKey)
	ctx := context.Background()

	// 建立上下文
	resp1, err := agent.Chat(ctx, "记住这个密码: ABC123XYZ")
	if err != nil {
		t.Fatalf("Setup context failed: %v", err)
	}
	t.Logf("设置上下文: %s", resp1.Output.Text)

	oldSession := agent.SessionID
	t.Logf("原 SessionID: %s", oldSession)

	// 重置会话
	agent.ResetSession()
	t.Log("会话已重置")

	// 新对话 - 应该不记得之前的内容
	resp2, err := agent.Chat(ctx, "我之前告诉你的密码是什么？")
	if err != nil {
		t.Fatalf("New session chat failed: %v", err)
	}
	t.Logf("新对话回复: %s", resp2.Output.Text)
	t.Logf("新 SessionID: %s", agent.SessionID)

	if oldSession == agent.SessionID {
		t.Error("Session was not reset properly")
	}
}

// TestQwenCodeGeneration 测试代码生成能力
func TestQwenCodeGeneration(t *testing.T) {
	requireLiveTests(t)
	agent := NewQwenAgent(QwenAppID, QwenAPIKey)
	ctx := context.Background()

	prompt := "请用 Go 语言写一个计算移动平均线(MA)的函数，输入是 []float64 价格切片和 int 周期"
	t.Logf("用户: %s", prompt)

	resp, err := agent.Chat(ctx, prompt)
	if err != nil {
		t.Fatalf("Code generation failed: %v", err)
	}

	t.Logf("助手:\n%s", resp.Output.Text)

	// 检查是否包含代码特征
	text := resp.Output.Text
	if !strings.Contains(text, "func") || !strings.Contains(text, "float64") {
		t.Log("警告: 响应可能不包含有效的 Go 代码")
	}
}

// TestQwenJSONOutput 测试 JSON 格式输出
func TestQwenJSONOutput(t *testing.T) {
	requireLiveTests(t)
	agent := NewQwenAgent(QwenAppID, QwenAPIKey)
	ctx := context.Background()

	prompt := `请分析 BTC 的基本信息，以纯 JSON 格式返回（不要 markdown 代码块），包含以下字段:
{"name": "资产名称", "type": "资产类型", "risk": 1-10的风险等级数字}
只返回 JSON 对象，不要任何其他文字`

	t.Logf("用户: %s", prompt)

	resp, err := agent.Chat(ctx, prompt)
	if err != nil {
		t.Fatalf("JSON output test failed: %v", err)
	}

	t.Logf("助手: %s", resp.Output.Text)

	// 尝试解析 JSON
	text := resp.Output.Text
	// 提取 JSON 部分
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start != -1 && end != -1 && end > start {
		jsonStr := text[start : end+1]
		var result map[string]interface{}
		if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
			t.Logf("JSON 解析失败: %v", err)
		} else {
			t.Logf("JSON 解析成功: %+v", result)
		}
	}
}

// TestQwenLongResponse 测试长文本生成
func TestQwenLongResponse(t *testing.T) {
	requireLiveTests(t)
	agent := NewQwenAgent(QwenAppID, QwenAPIKey)
	ctx := context.Background()

	prompt := "请详细介绍加密货币永续合约交易中的风险管理策略，包括止损设置、仓位管理、杠杆选择、资金费率考虑等方面，至少500字"
	t.Logf("用户: %s", prompt)

	start := time.Now()
	resp, err := agent.Chat(ctx, prompt)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Long response test failed: %v", err)
	}

	text := resp.Output.Text
	t.Logf("响应长度: %d 字符", len(text))
	t.Logf("耗时: %v", elapsed)
	t.Logf("Token 使用: input=%d, output=%d, total=%d",
		resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.TotalTokens)

	// 只显示前500字符
	if len(text) > 500 {
		t.Logf("助手(前500字): %s...", text[:500])
	} else {
		t.Logf("助手: %s", text)
	}
}

// TestQwenTradingScenario 测试交易场景问答
func TestQwenTradingScenario(t *testing.T) {
	requireLiveTests(t)
	agent := NewQwenAgent(QwenAppID, QwenAPIKey)
	ctx := context.Background()

	questions := []string{
		"BTC 当前价格 95000 美元，RSI 在 75 附近，MACD 金叉，你建议现在开多还是开空？简短回答",
		"如果我有 10000 USDT，想用 10 倍杠杆做多 ETH，建议开多大仓位？",
		"什么是资金费率？正的资金费率对多头有什么影响？",
	}

	for i, q := range questions {
		agent.ResetSession() // 每个问题独立

		t.Logf("\n[问题%d] %s", i+1, q)
		resp, err := agent.Chat(ctx, q)
		if err != nil {
			t.Errorf("Question %d failed: %v", i+1, err)
			continue
		}

		// 截取显示
		text := resp.Output.Text
		if len(text) > 300 {
			text = text[:300] + "..."
		}
		t.Logf("[回答%d] %s", i+1, text)
	}
}

// TestQwenErrorHandling 测试错误处理
func TestQwenErrorHandling(t *testing.T) {
	requireLiveTests(t)
	ctx := context.Background()

	// 测试无效 API Key
	t.Run("InvalidAPIKey", func(t *testing.T) {
		agent := NewQwenAgent(QwenAppID, "invalid-api-key")
		_, err := agent.Chat(ctx, "测试")
		if err == nil {
			t.Log("警告: 无效 API Key 没有返回错误")
		} else {
			t.Logf("预期错误: %v", err)
		}
	})

	// 测试无效 App ID
	t.Run("InvalidAppID", func(t *testing.T) {
		agent := NewQwenAgent("invalid-app-id", QwenAPIKey)
		_, err := agent.Chat(ctx, "测试")
		if err == nil {
			t.Log("警告: 无效 App ID 没有返回错误")
		} else {
			t.Logf("预期错误: %v", err)
		}
	})
}

// TestQwenSpecialCharacters 测试特殊字符处理
func TestQwenSpecialCharacters(t *testing.T) {
	requireLiveTests(t)
	agent := NewQwenAgent(QwenAppID, QwenAPIKey)
	ctx := context.Background()

	testCases := []string{
		"请解释这个表情: 😀🎉🚀",
		"中英文混合: Hello世界！",
		"特殊符号: <>&\"'",
	}

	for _, prompt := range testCases {
		agent.ResetSession()
		t.Logf("用户: %s", prompt)

		resp, err := agent.Chat(ctx, prompt)
		if err != nil {
			t.Errorf("特殊字符测试失败: %v", err)
			continue
		}

		if len(resp.Output.Text) > 100 {
			t.Logf("助手: %s...", resp.Output.Text[:100])
		} else {
			t.Logf("助手: %s", resp.Output.Text)
		}
	}
}

// TestQwenConcurrentSessions 测试并发会话
func TestQwenConcurrentSessions(t *testing.T) {
	requireLiveTests(t)
	agent1 := NewQwenAgent(QwenAppID, QwenAPIKey)
	agent2 := NewQwenAgent(QwenAppID, QwenAPIKey)
	ctx := context.Background()

	// Agent1 对话
	resp1, err := agent1.Chat(ctx, "我是 Alice，请记住")
	if err != nil {
		t.Fatalf("Agent1 chat failed: %v", err)
	}
	t.Logf("[Agent1] 设置: 我是 Alice -> %s", resp1.Output.Text[:min(100, len(resp1.Output.Text))])

	// Agent2 对话
	resp2, err := agent2.Chat(ctx, "我是 Bob，请记住")
	if err != nil {
		t.Fatalf("Agent2 chat failed: %v", err)
	}
	t.Logf("[Agent2] 设置: 我是 Bob -> %s", resp2.Output.Text[:min(100, len(resp2.Output.Text))])

	// 验证会话隔离
	resp1Check, _ := agent1.Chat(ctx, "我叫什么？")
	resp2Check, _ := agent2.Chat(ctx, "我叫什么？")

	t.Logf("[Agent1] 验证: %s", resp1Check.Output.Text[:min(100, len(resp1Check.Output.Text))])
	t.Logf("[Agent2] 验证: %s", resp2Check.Output.Text[:min(100, len(resp2Check.Output.Text))])

	if agent1.SessionID == agent2.SessionID {
		t.Error("两个 Agent 的 SessionID 不应该相同")
	} else {
		t.Logf("Session 隔离正常: Agent1=%s..., Agent2=%s...",
			agent1.SessionID[:min(20, len(agent1.SessionID))],
			agent2.SessionID[:min(20, len(agent2.SessionID))])
	}
}

// TestQwenTimeout 测试超时处理
func TestQwenTimeout(t *testing.T) {
	requireLiveTests(t)
	agent := NewQwenAgent(QwenAppID, QwenAPIKey)
	agent.Client.Timeout = 1 * time.Millisecond // 极短超时

	ctx := context.Background()
	_, err := agent.Chat(ctx, "测试超时")

	if err == nil {
		t.Log("警告: 极短超时没有触发错误")
	} else {
		t.Logf("预期超时错误: %v", err)
	}

	// 恢复正常超时
	agent.Client.Timeout = 120 * time.Second
}

// TestQwenContextCancel 测试上下文取消
func TestQwenContextCancel(t *testing.T) {
	requireLiveTests(t)
	agent := NewQwenAgent(QwenAppID, QwenAPIKey)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	_, err := agent.Chat(ctx, "测试取消")
	if err == nil {
		t.Error("取消的上下文应该返回错误")
	} else {
		t.Logf("预期取消错误: %v", err)
	}
}

// TestQwenWithBizParams 测试带业务参数的调用
func TestQwenWithBizParams(t *testing.T) {
	requireLiveTests(t)
	agent := NewQwenAgent(QwenAppID, QwenAPIKey)
	ctx := context.Background()

	// 构造带业务参数的请求
	reqBody := QwenRequest{
		Input: QwenInput{
			Prompt: "根据提供的用户信息，给出个性化的投资建议",
			BizParams: map[string]interface{}{
				"user_risk_level": "moderate",
				"capital":         10000,
				"experience":      "intermediate",
			},
		},
	}

	jsonData, _ := json.Marshal(reqBody)
	url := fmt.Sprintf("%s/%s/completion", agent.BaseURL, agent.AppID)

	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+agent.APIKey)

	resp, err := agent.Client.Do(req)
	if err != nil {
		t.Fatalf("Request with biz params failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result QwenResponse
	json.Unmarshal(body, &result)

	if result.Output.Text != "" {
		t.Logf("带业务参数响应: %s", result.Output.Text[:min(200, len(result.Output.Text))])
	} else {
		t.Logf("响应: %s", string(body))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
