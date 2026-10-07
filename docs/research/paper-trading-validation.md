# Paper Trading 验证报告：DeepSeek on bitget_paper

> **风险声明**：本文是工程链路验证记录，**不构成任何投资建议**，也不是策略收益的证明。下文的盈亏数字来自约 2 小时、两个交易员、几十个决策周期的样本，统计上没有意义，只用来说明"下单 → 止盈止损 → 平仓 → 落库"这条链路能走通。带杠杆的实盘交易可能损失全部本金，LLM 的决策不保证盈利。上实盘之前，请先完成本文「未验证项」中的 Bitget 官方 Demo 验证，再考虑极小资金。

- 分支：`ccr-859741ef-19y2r9`
- 运行窗口：2026-10-07 11:28 → 约 13:50 UTC（约 2 小时 20 分钟，周三；TradFi 合约处于开市状态，但多数时间在美股常规交易时段之外）
- 相关文档：
  - 调研与路线图：[`docs/research/alpha-arena-llm-trading-bitget.md`](alpha-arena-llm-trading-bitget.md)
  - `bitget_paper` 使用说明：[`docs/bitget-paper-trading.md`](../bitget-paper-trading.md)

## 1. 摘要

| 项目 | 结论 |
|---|---|
| 目标 | 验证调研文档中 P0–P2 的工程修复，在没有 Bitget Demo Key 的情况下，用真实 LLM 和真实 Bitget 行情跑通 Alpha Arena 式的完整交易链路 |
| 总体结果 | 两个 DeepSeek 交易员合计 92 个决策周期，91 个成功，1 个失败（已定位并修复）；日志无 panic |
| TradFi | NVDAUSDT、XAUUSDT 的行情、K 线、持仓量、资金费率均来自 Bitget，并成功以合约精度开平仓 |
| 止盈 | 模拟撮合器触发 2 次止盈，订单、成交、持仓平仓三类记录完整落库 |
| 重启 | 运行期间后端重启 4 次；修复后重启能恢复账户余额与持仓，且与重启前一致 |
| 发现并修复的缺陷 | 4 个缺陷加 10 条评审问题（第 6 节）；其中 3 个缺陷是跑真实 LLM 的 paper trading 才暴露的 |
| 未验证 | Bitget 官方 Demo（paptrading）与实盘私有 API 行为（第 7 节） |

## 2. 范围与提交清单

本轮工作是 nofx 上的工程修复，目标是支撑 Alpha Arena 式 LLM 交易系统（Bitget，BTC/ETH + TradFi 永续）。验证手段是用真实 LLM（DeepSeek，模型 ID `deepseek-chat`，当前 API 实际服务的是 DeepSeek-V4.1-Flash）在本地模拟交易所上做 paper trading。

所有提交都在分支 `ccr-859741ef-19y2r9` 上：

| 提交 | 摘要 |
|---|---|
| `d3f6d56` | fix(bitget)：单向持仓语义（不再传 `tradeSide`，平仓用 `reduceOnly`）；仓位级止盈止损改走 `place-tpsl-order`；按标的设置保证金模式；设置杠杆失败时中止开仓；通过 `order/detail` 确认成交；数量向下取整并检查最小数量和最小名义价值；新增 `GetOpenOrders`；修正 `GetOrderStatus` / `GetClosedPnL` 的 v2 字段；成交历史分页与 `feeDetail`；Demo（`paptrading` 请求头）开关；修复原有交易器测试的编译错误 |
| `2449554`（合入于 `06efd42`） | feat(market)：新增 Bitget 公开行情 provider（`provider/bitget`）和 `market.Source` 路由；TradFi 资产类别（crypto / equity / commodity / fx，由 `isRwa` 判定）；周一至周五 24 小时的交易时段规则（`America/New_York`）；按类别的杠杆上限（默认股票 5x、商品 10x，且不超过合约 `maxLever`）；提示词中加入标的信息行 |
| `f2cc0b1` | fix(kernel)：休市期间的 TradFi 开仓决策被单独丢弃，不再使整个周期失败 |
| `63e3a05` | feat(trader)：`bitget_paper` 本地模拟交易所，基于 Bitget 实时价格（费率取自合约、2 bps 滑点、止盈止损、近似强平、资金费率），并接入 API 与 UI |
| `5513ad0` | fix(trader)：策略模式下交易员级自定义提示词被静默忽略（paper 运行中发现） |
| `390225c` | fix(paper)：模拟账户跨重启持久化（原子 JSON 快照）；启动时对账孤儿 DB 持仓；超额 `position_size_usd` 改为截断而不是作废整个周期（均为 paper 运行中发现） |
| `e4eaffb` | fix：代码评审的 10 条问题（见第 6 节） |

## 3. 验证环境与方法

### 3.1 部署方式

后端由该分支构建，无界面启动（SQLite，`TRANSPORT_ENCRYPTION=false`），全程只通过 REST API 用脚本配置：

1. 注册账号并完成 TOTP 绑定
2. `PUT /api/models`：配置 DeepSeek 模型
3. `POST /api/exchanges`：`exchange_type` 为 `bitget_paper`（无需任何 Key）
4. `POST /api/strategies`：使用默认策略配置，`coin_source` 为静态列表：`BTCUSDT`、`ETHUSDT`、`NVDAUSDT`、`TSLAUSDT`、`XAUUSDT`
5. `POST /api/traders`：初始资金 10000，扫描间隔 3 分钟
6. `POST /api/traders/:id/start`

### 3.2 两个交易员

两个交易员使用同一个模型、同一个交易所类型，各 10,000 USDT 本金：

| 交易员 | 提示词 | 目的 |
|---|---|---|
| `deepseek-bitget_paper` | 默认策略提示词 | 保守基线，观察模型在默认约束下的自然行为 |
| `deepseek-paper-active` | 同一策略 + 交易员级自定义提示词，要求每次用小仓位（300–800 USDT、2–3x 杠杆）轮换交易 | 提高交易频率，把开仓、平仓、落库链路都跑到。最后一个阶段把提示词改为极窄止损/止盈（0.1%–0.2%）且不手动平仓，用来触发交易所侧的止盈止损 |

后端在运行期间重启了 4 次（用于部署修复），同时起到了重启与持久化测试的作用。

## 4. 运行结果

### 4.1 汇总

| | `deepseek-bitget_paper` | `deepseek-paper-active` |
|---|---|---|
| 决策周期 | 48（1 个失败：超额仓位缺陷，已在 `390225c` 修复） | 44（0 个失败） |
| AI 延迟 | 中位 7.2s，p90 10.9s，最大 12.6s | 中位 5.1s，p90 6.8s，最大 10.9s |
| 动作分布 | open_short 7，close_short 3，hold 46，wait 28 | open_short 17，open_long 1，close_short 12，close_long 1，hold 53，wait 9 |
| 持仓数 | 7（BTC 3，ETH 2，XAU 2）；其中 3 个由启动对账关闭（`paper_state_lost`，发生在持久化修复之前的那次重启） | 18（BTC 6，NVDA 5，XAU 4，ETH 3）；其中 2 个由启动对账关闭 |
| 已实现盈亏（不含对账关闭的仓位） | +57.68 USDT，2 胜 1 负 | +4.26 USDT，7 胜 8 负 |
| 权益 | 10000 → 9983.60（最低 9884.52，最高 10026.11） | 10000 → 9997.88（最低 9991.52） |

说明：

- 两个交易员合计 92 个周期、25 个持仓记录。启动对账共关闭 5 个孤儿持仓（3 + 2）。
- "已实现盈亏"取自持仓记录，"权益"取自账户余额与持仓估值，两者口径不同，不能直接相减。两者差额的构成（手续费、滑点、浮动盈亏）本报告没有逐项核对。
- 基线交易员的动作几乎全是做空，活跃交易员也以做空为主。本报告不对这一偏向做解读。

### 4.2 交易所侧止盈止损

| 项目 | 结果 |
|---|---|
| 止盈触发 | 模拟撮合器触发 2 次。例：BTCUSDT 空单，数量 0.0071，开仓价 83410.71，止盈成交价 82860.07，盈亏 +3.91 |
| 记录完整性 | 每次触发都完整写入：订单记录、成交记录、持仓平仓（DB），端到端一致 |
| 止损触发 | 本次运行中没有实盘触发，由单元测试覆盖 |
| 强平 | 本次运行中没有发生，由单元测试覆盖 |
| 资金费率结算 | 运行窗口内没有跨过 8 小时结算点，由单元测试覆盖 |

### 4.3 TradFi 链路（实盘行情）

| 检查项 | 结果 |
|---|---|
| 行情来源 | NVDAUSDT / XAUUSDT 的价格、K 线、持仓量、资金费率均来自 Bitget，没有走 Hyperliquid 的 `xyz:` 映射。例如第 1 个周期 NVDA 价格 237.97 |
| 提示词标的信息行 | 实际写入：`Instrument: class=equity \| session=OPEN \| open, outside US regular hours ... \| max leverage allowed=5x` |
| 合约精度 | NVDA 开平仓数量 2.52 / 2.1；XAU 数量 0.14 / 0.12，均符合合约数量精度 |
| 杠杆上限 | 股票类按 5x 限制，且不超过合约 `maxLever` |

### 4.4 持久化与重启

| 场景 | 结果 |
|---|---|
| 持久化修复之前重启 | 模拟账户丢失，DB 中的持仓仍为 OPEN（孤儿） |
| 启动对账 | 5 个孤儿持仓被关闭，`close_reason` 为 `paper_state_lost` |
| 持久化修复之后带持仓重启 | 两个账户都恢复成功，钱包余额与持仓与重启前一致 |
| 快照 v1 → v2 迁移 | 在真实重启中验证通过 |

### 4.5 日志

- panic：0
- nofxos 数据接口：整个运行期间累计 1,358 行错误日志（见第 7.3 节），不影响下单

## 5. 验证覆盖矩阵

| 能力 | 真实 LLM + 实盘行情 | 单元测试 / fixture | 备注 |
|---|---|---|---|
| Bitget 公开行情（K 线、OI、资金费率、合约信息） | 已验证 | 有 | 含 TradFi |
| TradFi 提示词、杠杆上限、合约精度 | 已验证 | 有 | |
| 休市期间丢弃单个开仓决策 | 未触发 | 有 | 运行窗口内合约均为开市状态 |
| 模拟开仓 / 平仓 / 落库 | 已验证 | 有 | |
| 模拟止盈 | 已验证（2 次） | 有 | |
| 模拟止损、强平 | 未触发 | 有 | |
| 模拟资金费率结算 | 未触发 | 有 | 窗口内无 8 小时边界 |
| 快照持久化、对账、v1→v2 迁移 | 已验证 | 有 | |
| 交易员自定义提示词 | 已验证 | 有 | `5513ad0` |
| Bitget 实盘 / Demo 私有 API | **未验证** | 有（fixture） | 见第 7.1、7.2 节 |

## 6. 运行中发现的缺陷

| # | 症状 | 根因 | 修复提交 |
|---|---|---|---|
| 1 | 策略引擎模式下，交易员的 `custom_prompt` 完全不生效 | `AutoTrader` 保存了该字段，但从未传给 `StrategyEngine` | `5513ad0` |
| 2 | 后端重启后模拟账户丢失，数据库里的持仓永远是 OPEN | 模拟账本只存在于内存 | `390225c`（原子 JSON 快照 + 启动对账） |
| 3 | DeepSeek 对 NVDA 给出 25,000 USDT 的仓位（上限 9,978），整个周期作废，连同其中的 hold 和平仓决策一起丢失 | 校验对超额仓位直接返回错误，而不是截断 | `390225c`（截断到上限） |
| 4 | 休市期间的 TradFi 开仓使整个周期作废（评审中发现，运行前已处理） | 该决策没有被单独丢弃，而是让整个周期失败 | `f2cc0b1`；在 `e4eaffb` 中扩展到未知标的的开仓 |
| 5 | 代码评审的 10 条问题 | 见下表 | `e4eaffb` |

缺陷 1–3 是用真实 LLM 跑 paper trading 才暴露的，单元测试和 fixture 没有覆盖到。

`e4eaffb` 涉及的评审问题（主要内容，不是逐条对应的完整清单；未知标的的开仓同样改为单独丢弃，见缺陷 4）：

| 类别 | 内容 |
|---|---|
| 止盈止损 | 替换止盈止损失败时恢复原单 |
| 成交分类 | 一进一出刚好持平（break-even）时的单向持仓成交分类 |
| 资金费率 | 结算标记在 `fundInterval` 变化后仍然稳健（快照升级到 v2，兼容 v1 迁移） |
| 快照 | 处理保证金为负的快照 |
| K 线粒度 | 支持 Bitget 的 2h、3d 粒度 |
| 提示词 | 支持 `override_base_prompt`，并修复一处数据竞争 |
| UI | 展示实际生效的杠杆上限 |
| 合约缓存 | 批量加载与负缓存 |
| 重复代码 | 合并重复的 Bitget K 线与数据源映射逻辑 |

## 7. 已知问题与未验证项

### 7.1 Bitget 官方 Demo（paptrading）未运行

- 环境中没有 Demo API Key，因此没有跑过 Demo。
- 仓库里有 `TestBitgetDemoIntegration`，设置 `BITGET_DEMO_API_KEY`、`BITGET_DEMO_SECRET_KEY`、`BITGET_DEMO_PASSPHRASE` 后才会执行，否则跳过。
- Bitget 的 Demo 对部分账户可能使用独立的币种和产品类型（例如 `SUSDT`、`SBTCSUSDT`）。正式使用前先用一笔最小订单确认下单、止盈止损和持仓查询都正常。

### 7.2 Bitget 私有 API 的真实响应未对照

止盈止损接口（`place-tpsl-order`）、单向持仓下成交的 `tradeSide` 取值、历史持仓字段，目前是依据 SDK、ccxt 源码和 fixture 实现的，**还没有用真实响应确认**。这部分风险只能通过 7.1 的 Demo 或极小实盘订单消除。

### 7.3 nofxos 数据接口的密钥已被上游弃用

- nofx 内置的 nofxos 数据 API Key 已被上游废弃。每个周期里量化数据、OI 排名、资金净流入、价格排名都会报错（本次运行共 1,358 行错误日志）。
- 交易照常进行，这些章节只是空的，但每个周期会多花约 5 秒。
- 建议：在 Bitget 策略里关闭这些指标，或更换为有效的 Key。

### 7.4 其他

| 问题 | 说明 |
|---|---|
| `close_reason` 不够细 | 模拟盘止盈止损平仓在 DB 中的 `close_reason` 记为通用的 `sync`；具体原因只在日志和订单记录里 |
| 美国节假日 | 交易时段日历没有建模美国市场假日 |
| 原有测试失败（与本工作无关） | kernel 的 `TestDataDictionary`、`TestTradingRules`；manager 的 `TestRemoveTrader`（空指针）；trader 的 `TestBybitTrader_FormatQuantity`、`TestNewHyperliquidTrader`（需联网） |
| 原有构建问题 | `go build ./...` 在 `scripts/` 下失败（多个 `main`），在本工作之前就存在 |
| 样本量 | 约 2 小时、两个交易员，不足以评价策略或模型的收益、回撤和风险 |

## 8. 如何复现

### 8.1 运行 bitget_paper

通过界面：

1. 「AI 交易员」页面 → 添加交易所 → 类型选 **Bitget Paper（本地模拟）**，不需要任何 Key
2. 配置任意 AI 模型和策略
3. 创建交易员，选择该交易所，填写初始资金（不填或 ≤0 时默认 10000 USDT）
4. 启动交易员

通过 API（流程与第 3.1 节一致）：注册并绑定 TOTP → `PUT /api/models` → `POST /api/exchanges`（`exchange_type: bitget_paper`）→ `POST /api/strategies` → `POST /api/traders` → `POST /api/traders/:id/start`。模型可以换成任意受支持的提供方，不限于 DeepSeek。

模拟账户快照位于 `<数据库目录>/paper/<交易员ID>.json`，细节见 [`docs/bitget-paper-trading.md`](../bitget-paper-trading.md)。

### 8.2 运行测试

```bash
# 交易器：模拟盘与 Bitget（带竞态检测）
go test -short -race ./trader/ -run 'Paper|Bitget'

# 决策内核、行情、Bitget provider
go test -short ./kernel/ ./market/ ./provider/bitget/

# 联网的实时数据测试（需要能访问 Bitget 公开接口）
go test ./provider/bitget/ ./market/ -run Live -v

# 官方 Demo 集成测试（需要 Demo Key，三个环境变量未设置时跳过）
BITGET_DEMO_API_KEY=... BITGET_DEMO_SECRET_KEY=... BITGET_DEMO_PASSPHRASE=... \
  go test -count=1 -v ./trader/ -run TestBitgetDemoIntegration
```

第 7.4 节列出的失败测试是原有问题，运行整个包时会出现。

## 9. 建议的下一步

| 优先级 | 事项 |
|---|---|
| 高 | 取得 Bitget Demo Key，跑 `TestBitgetDemoIntegration` 和一次最小订单，核对止盈止损、成交分类与持仓历史字段的真实响应 |
| 高 | 在 Bitget 策略中关闭已失效的 nofxos 指标，或更换有效的 Key，去掉每周期约 5 秒的开销 |
| 中 | 把模拟盘止盈止损的平仓原因写入 `close_reason`，替代通用的 `sync` |
| 中 | 延长运行时间并覆盖一个完整的资金费率结算边界、一次休市 → 开市切换，以及一次止损触发 |
| 低 | 建模美国市场假日 |
