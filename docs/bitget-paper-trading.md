# Bitget Paper：本地模拟交易所

`bitget_paper` 是 NOFX 内置的本地模拟撮合交易所。下单、持仓、止盈止损、强平和资金费率都在本进程内模拟（账户状态每次变化后落盘，重启不丢，见「状态保存与重置」），
价格取自 Bitget 实时公开行情（USDT 永续，包含美股/商品/外汇等 TradFi 合约）。
它不需要任何 API Key，也不会向 Bitget 发送任何交易请求，只读取公开的行情和合约接口。

适合场景：

- 不动用真实资金，先跑通 AI 决策 → 下单 → 止盈止损 → 复盘的完整链路。
- 用同一份 Bitget 行情对比不同模型和提示词（行情源与实盘 `bitget` 交易所一致）。
- 在没有 Demo Key 时做离线可复现的回归测试。

## 如何启用

1. 打开「AI 交易员」页面，点「添加交易所」，类型选 **Bitget Paper（本地模拟）**，填一个账户名称并保存。该类型不需要 API Key、Secret 和 Passphrase。
2. 创建交易员时选择这个交易所账户，并在「初始资金」里填写模拟本金（不填或 ≤0 时默认 10000 USDT）。
3. 策略配置、AI 模型、扫描间隔与其他交易所一致。启动交易员后，每个周期照常调用模型决策，订单由本地撮合器成交。

行情数据源会自动使用 Bitget（K 线、持仓量、资金费率），TradFi 标的（如 `NVDAUSDT`、`XAUUSDT`）不会再被映射到 Hyperliquid 的 `xyz:` 名称。

## 模拟模型

| 项目 | 规则 |
| --- | --- |
| 价格 | 使用合约 ticker 的标记价（markPrice），缺失时回退到最新价。所有成交、止盈止损触发都基于它。 |
| 成交 | 只支持市价单，立即全部成交。成交价在标记价基础上向不利方向偏移 2 bps：买入更贵，卖出更便宜。 |
| 手续费 | 每笔成交按 taker 费率收取，取合约的 `takerFeeRate`，拿不到时用 0.06%。 |
| 数量 | 按合约 `sizeMultiplier` 向下取整。开仓数量低于 `minTradeNum` 或名义价值低于 `minTradeUSDT` 会被拒绝；平仓（reduce-only）不做最小值检查，保证残余仓位总能平掉。 |
| 杠杆 | 每个标的单独设置，超过合约 `maxLever` 时自动截到上限。已有仓位保持开仓时的杠杆。 |
| 保证金 | 开仓锁定 `名义价值 / 杠杆`，并先扣除手续费。可用余额不足以覆盖「保证金 + 手续费」时拒单。 |
| 持仓模式 | **新建**的交易所账户默认**双向持仓（hedge）**，与实盘 `bitget`、Binance、OKX 一致（升级前已创建的账户保持 `one_way`，见下面的「持仓模式（双向 / 单向）」）：同一标的可以同时持有多单和空单，两个仓位各有自己的开仓价、保证金、止盈止损、强平和资金费率。可选**单向持仓（one_way）**：同一标的只有一个方向，持有多单时开空单会报错（自动交易器会先平仓）。两种模式下同方向加仓都按加权平均更新开仓价。详见下面的「持仓模式（双向 / 单向）」。 |
| 保证金模式 | `SetMarginMode` 只做记录。无论全仓还是逐仓，强平判断都只看该仓位自己的保证金，可用余额不会为仓位补保证金。 |
| 休市 | 美股/商品/外汇合约在周六 00:00 到周一 00:00（美东）休市，休市时拒绝开仓，平仓不受限制；合约状态不是 `normal` 时同样拒绝开仓。加密货币 7×24 可交易。美国节假日（NYSE 2025–2027 静态日历，含补休日和 13:00 ET 提前收盘日）只做标注、不拦截：Bitget 股票永续在假日照常 5×24 交易，所以假日和提前收盘后 `regularHours=false`，提示词标的信息行里的 note 会写明“US cash market closed (Thanksgiving)”或“US early close 13:00 ET”，并提醒流动性可能偏薄、复牌时价格可能跳空。真正的停牌仍以合约状态为准。日历之外的年份 note 会注明节假日未模拟。 |

### 持仓模式（双向 / 单向）

- 模式由交易所账户的「持仓模式」设置决定（交易所配置里的下拉框，后端字段 `bitget_position_mode`，取值 `hedge` 或 `one_way`；`bitget` 与 `bitget_paper` 共用这个设置）。
  交易员加载时把设置应用到模拟账户。
- **默认值**：新建的交易所账户默认 `hedge`。**引入这个设置之前已创建的交易所账户保持 `one_way`**（那时 nofx 强制单向），升级时数据库迁移一次性把它们写成 `one_way`（SQLite 与 PostgreSQL 都是），所以升级不会改变已有账户的行为；想用双向持仓，需要在交易所配置里手动改成 `hedge`（空仓时才会生效）。迁移只在添加该列时执行一次，之后不会再覆盖你的选择。
- **只有账户空仓时才能切换**（与真实 Bitget 一致：有持仓或挂单时无法切换持仓模式）。有持仓时设置不会生效，日志里会有一条警告，账户继续沿用当前模式；全部平仓并重启交易员后才会应用新设置。
- 双向持仓下：
  - 持仓以「标的 + 方向」为键。`GetPositions` 对同一标的返回两条记录（`long`、`short`）。`CloseLong` / `CloseShort` 只影响对应方向。
  - 止盈止损按「标的 + 方向」独立设置、独立触发：多单的止损触发只平多单，空单不受影响。`SetStopLoss` / `SetTakeProfit` 的 `positionSide` 指明方向；同一标的多空都持有时必须指明，否则报错。
  - 强平和资金费率按仓位单独计算：高杠杆空单被强平不会连带多单；多单付资金费时同标的的空单收取（各自结算，互不影响）。
  - `CancelStopLossOrders` / `CancelTakeProfitOrders` / `CancelStopOrders` / `CancelAllOrders`（接口里没有方向参数）会取消该标的两个方向的单；需要只取消一个方向时用 `CancelStopLossOrdersForSide` / `CancelTakeProfitOrdersForSide` / `CancelStopOrdersForSide`。
- 单向持仓下行为与引入双向持仓之前完全一致（拒绝反向开仓，每个标的最多一个仓位）。

### 止盈止损

- 每个标的的每个方向只有一个仓位级止损和一个仓位级止盈，重复设置会替换旧的。仓位完全平掉后自动删除。
- 下单时与交易所一致地校验方向：多单止损必须低于当前标记价、止盈必须高于；空单相反。价格会按合约的价格精度取整。
- 触发条件基于标记价（多单：`mark <= 止损` 或 `mark >= 止盈`；空单相反），触发后按市价平掉整个仓位，同样带滑点和手续费。
- `GetOpenOrders` 返回它们，类型为 `STOP_MARKET` / `TAKE_PROFIT_MARKET`，与实盘 Bitget 交易器一致。

### 强平（近似）

当 `仓位保证金 + 未实现盈亏 <= 名义价值 × 0.5%`（名义价值按标记价计算）时强平。
强平按标记价成交，仓位剩余的权益全部作为强平费用没收，亏损最多等于该仓位的保证金。
没有模拟 Bitget 的分档维持保证金、强平清算价与保险基金，实际强平价会与这里不同，仅用于估算风险。

### 资金费率

按合约 `fundInterval`（默认 8 小时）在 UTC 整点边界结算，8 小时即 00:00 / 08:00 / 16:00。
结算金额 = 名义价值（标记价）× 当前资金费率；费率为正时多头支付、空头收取，费率为负时反过来。
金额直接加减到该仓位的保证金上（因此也会影响强平判断），平仓时随保证金一并结回账户。
使用的是结算时刻的「当前资金费率」，不是交易所事后确定的历史费率。

### 后台撮合

交易员启动（或第一次开仓）时会启动一个后台协程，默认每 5 秒检查一次：资金费率结算、强平、止损、止盈（按这个顺序）。
没有持仓时检查是空操作，不会请求行情。仅用于查询余额的临时实例（例如创建交易员时探测初始资金）不会启动协程。
交易员停止时撮合协程随之停止：账户和仓位保留（内存中并已落盘）但处于冻结状态，
重新启动后第一次检查会按当时的标记价判断止盈止损，并补结算期间错过的资金费率。

## 订单与持仓记录

- AI 下单走和其他无 OrderSync 交易所相同的路径：下单后立即写订单记录，再通过 `GetOrderStatus` 取成交价、数量和手续费并写入成交和持仓。`bitget_paper` 没有加入 OrderSync 跳过列表，所以订单会被正常记录。
- 止盈、止损、强平由撮合器自己触发，它们通过 `recordPaperSystemFill` 写入同样的订单、成交和持仓平仓记录。
- `GetClosedPnL` 返回平仓记录：`RealizedPnL` 是不含手续费的价格盈亏，`Fee` 是开仓与平仓手续费之和（强平时包含被没收的权益），`CloseType` 为 `manual`、`stop_loss`、`take_profit` 或 `liquidation`。资金费率不单独出现在平仓记录里，但会反映在余额上。

## 状态保存与重置

### 落盘与重启恢复

- 每个交易员的模拟账户按交易员 ID 保存为一个 JSON 快照文件，路径为 `<数据库文件所在目录>/paper/<交易员ID>.json`。
  默认的 SQLite 路径 `data/data.db` 对应 `data/paper/<交易员ID>.json`；`DB_PATH` 改变时目录随之改变；使用 PostgreSQL 时固定为 `data/paper/`（相对启动目录）。
  交易员 ID 里不安全的字符会被替换为 `_` 并追加一段哈希，避免不同 ID 写到同一个文件。
- 快照内容：余额（可用资金和初始资金）、累计已实现盈亏、全部持仓（方向、数量、开仓价、杠杆、保证金、开仓时间、资金费率结算进度）、
  持仓上的止盈止损单、每个标的的杠杆与保证金模式设置、最近 500 条平仓记录和最近 500 笔成交（供 `GetOrderStatus` 与 `GetClosedPnL` 使用）、
  下一个订单序号，以及每个持仓的资金费率结算时间点（Unix 秒，按结算边界计算，`fundInterval` 变化时也不会多扣）。快照带 `"version": 3`，并记录 `"position_mode"`（`hedge` / `one_way`）；持仓列表里同一标的可以出现 long 和 short 两条（双向持仓）。
  读取旧快照时会自动迁移：v1 → v2 把各持仓的资金费率结算进度重置为快照保存时间；v2 → v3 因为双向持仓出现之前所有账户都是单向的，**有持仓的账户按 `one_way` 迁移以保持原有行为**（不会突然允许反向开仓；全部平仓并重启交易员后才会应用交易所设置里的模式），空账户没有需要保留的行为，直接采用交易所设置里的模式（新建账户默认 `hedge`，升级前已创建的账户是 `one_way`）。
- 每次状态变化（开仓、平仓、设置或取消止盈止损、杠杆与保证金模式变化、资金费率结算、止盈止损与强平成交）都会立即写盘：
  先写同目录下的临时文件，`fsync` 后原子重命名覆盖，所以不会出现写到一半的文件，也不会留下临时文件。
  写盘失败只记录警告，不会让这笔交易失败；下一次状态变化和交易员停止时会重试。
- 进程重启后，创建交易员（`AcquireBitgetPaperTrader`）时会读取快照并恢复账户。只有没有快照时才会使用交易员的「初始资金」作为本金。
  恢复后撮合协程随交易员启动，第一次检查会按当时的标记价判断止盈止损，并补结算停机期间错过的资金费率（与交易员停止再启动时的行为一致）。
- 快照无法解析、版本号不在 1–3 范围内、或内容不合法（例如数量为负、方向未知、属于别的交易员）时会被忽略，账户按全新账户创建，并在日志里给出警告。
  这类文件不会被覆盖，而是改名为 `<交易员ID>.json.rejected` 保留，便于排查。
- 删除交易员会释放账户并删除它的快照文件。仅停止、启动或修改交易员配置不会重置账户。
- 想手动重置某个模拟账户：停止后端，删除对应的 `paper/<交易员ID>.json`，再启动；启动对账（见下）会把数据库里遗留的 OPEN 仓位关闭。

### 启动对账

交易员每次启动（`Run`）时，会拿数据库里该交易员所有 OPEN 的持仓和模拟账户的持仓（按标的 + 方向）比对。
数据库里是 OPEN、但模拟账户里没有的仓位（典型原因：升级到支持落盘之前留下的仓位、快照文件被删除或损坏），
会被自动平掉并打印警告日志：

- 平仓价为当前标记价（取不到时用开仓价），已实现盈亏按该价格计算，不额外收取手续费。
- `close_reason` 为 `paper_state_lost`，数据库里的持仓记录变为 CLOSED，不再留下永远 OPEN 的孤儿记录，AI 与界面看到的状态保持一致。
- 模拟账户里有、数据库里没有 OPEN 记录的仓位只记录警告，不做修改。
- 对账只对 `bitget_paper` 交易员执行，实盘交易所不受影响。

注意：这笔盈亏只写在数据库的持仓记录上，模拟账户本身已经丢失了对应的仓位，余额不会因此变化。

### 其他说明

- 修改交易员的「初始资金」不会改变已存在的模拟账户余额，只影响收益率的基线。
- 快照目录由 `trader.SetPaperStateDir` 设置，`main.go` 在加载交易员之前根据数据库配置调用它；不设置时（例如单元测试）账户只在内存里，不落盘。

## 没有模拟的部分

订单簿深度与随下单量变化的滑点、限价单与部分成交、分档杠杆与分档维持保证金、保险基金与自动减仓（ADL）、手续费阶梯和返佣、交易所维护窗口、美国节假日、全仓模式下可用余额为仓位补保证金。

## 实时冒烟测试

仓库里有一个默认跳过的联网测试（需要设置 `NOFX_LIVE_TESTS=1` 才会运行，CI 不会运行它），会用真实 Bitget 公开行情开一笔约 10 USDT 名义价值的 BTCUSDT 多单、设置止盈止损、平仓并打印余额：

```bash
NOFX_LIVE_TESTS=1 go test -count=1 -v ./trader/ -run TestPaperLiveSmoke
```

## 使用 Bitget 官方 Demo（需要 Demo API Key）

如果希望用 Bitget 官方模拟盘撮合（而不是本地模型），使用现有的 `bitget` 交易所并打开 Demo 开关：

1. 在 Bitget 网站切换到模拟交易（Demo Trading），创建 **Demo API Key**，记下 API Key、Secret Key 和 Passphrase。
2. 在 NOFX 添加交易所，类型选 **Bitget Futures**，填入上面的 Demo Key，并打开 **Demo 模拟交易 (paptrading)** 开关（即交易所的 testnet 开关，后端字段 `testnet`）。
3. 开启后，所有请求会带上 `paptrading: 1` 请求头。该模式下真实 API Key 会被 Bitget 拒绝，Demo Key 也不能用于实盘。

### 实盘 / Demo 的持仓模式（双向 hedge / 单向 one_way）

`bitget` 交易所（含 Demo）同样使用交易所账户上的「持仓模式」设置（`bitget_position_mode`；`bitget_paper` 共用同一个设置）。**新建的交易所账户默认 `hedge`；升级前已创建的账户保持 `one_way`，直到你在 UI 里改掉它**（升级迁移一次性写入，之后不再覆盖）。以下已在 Bitget Demo 上逐项验证：

- **检测与切换**：交易员创建时先用 `GET /api/v2/mix/account/account?symbol=BTCUSDT&productType=USDT-FUTURES&marginCoin=USDT` 读取账户当前的 `posMode`（`hedge_mode` / `one_way_mode`；账户列表接口 `accounts` 不返回它），与设置不一致时调用 `POST /api/v2/mix/account/set-position-mode` 切换。
  Bitget 只允许在**没有持仓、也没有挂单（含止盈止损计划单）**时切换，否则返回 `40920 Position or order exists, the position mode cannot be switched`。
  切换失败时交易员**按检测到的当前模式继续运行**，并在日志里给出明确警告；平掉所有仓位和挂单并重启交易员后设置才会生效。重复设置为相同模式返回成功。
- **下单格式**（hedge）：`side` 是**仓位方向**（多单 `buy`、空单 `sell`），`tradeSide` 为 `open` / `close`。开多 `buy+open`，平多 `buy+close`，开空 `sell+open`，平空 `sell+close`。
  开仓单不发 `reduceOnly`（Demo 实测：hedge 下带 `reduceOnly: YES` 的开仓单仍然正常开仓，该字段被忽略）；**平仓单额外带 `reduceOnly: YES` 作为兜底**（hedge 下被接受并忽略，实际由 `tradeSide=close` 决定）。
  这样如果交易员对账户模式的判断错了，把 hedge 格式的平仓单发给了 one-way 账户，订单会因 reduce-only 被拒绝（one-way 账户本来就会因 `tradeSide` 返回 `40774`），而不会变成一笔加仓。
  one-way 下 `side` 是真实下单方向（平多 = `sell`），平仓用 `reduceOnly: YES`，且**不能**发 `tradeSide`。
- **模式不匹配**：下单格式与账户模式不符时 Bitget 返回 `40774 The order type for unilateral position must also be the unilateral position type.`（两个方向文案相同）。
  交易员收到后会重新检测账户模式、更新缓存，并**按新模式重新构造订单**（新的 `clientOid`）**重试一次**；检测失败时按相反模式重试。`40774` 是下单前的参数校验拒绝，订单没有进入撮合，因此重试不会重复下单；其它任何错误（超时、系统错误、余额不足等）都不会重试。
- **止盈止损**：`place-tpsl-order` 的 `holdSide` 在 hedge 下是 `long` / `short`，在 one-way 下是 `buy`（多）/ `sell`（空）；用错词汇返回 `43011 ... holdSide error`（one-way 收到 long/short）或 `43011 ... delegateType is error`（hedge 收到 buy/sell），交易员同样会刷新模式并重试一次。
  同一 `holdSide` 重复设置 `pos_loss` / `pos_profit` 是原地替换（orderId 不变，非法价格被拒绝时原单保持不变）；多空两侧的止盈止损互相独立。
  持仓平掉后该侧的止盈止损随之消失，另一侧不受影响。挂单列表里 hedge 的计划单 `posSide` 是 `long` / `short`、`tradeSide` 是 `close`。
- **取消止盈止损**：`cancel-plan-order` 必须传计划单**自己的** `planType`（`pos_loss` / `pos_profit`）。传列表接口用的 `profit_loss` 时 Bitget 返回成功但 `successList` 为空、什么也没取消（one-way 与 hedge 都一样）。
  交易员按 planType 分组取消，并要求每个 id 都出现在 `successList` 中，否则报错。接口没有方向参数时 `CancelStopLossOrders` / `CancelTakeProfitOrders` / `CancelStopOrders` 取消该标的两侧；需要只取消一侧时用 `CancelStopLossOrdersForSide` / `CancelTakeProfitOrdersForSide` / `CancelStopOrdersForSide`。
- **持仓与成交**：`all-position` 在 hedge 下对同一标的返回 `holdSide=long` 和 `holdSide=short` 两条。两条都开着时，Bitget 对两侧都返回荒谬的强平价（`60549679693.56`，约为标记价的 72 万倍；多空风险对冲），交易员把它当作「不适用」（0）。
  强平价的合理性检查：任何仓位，强平价 ≤ 0、NaN/Inf、高于标记价 100 倍或低于标记价 1/100 都当作「不适用」（0）。**方向检查（多单强平价必须低于标记价、空单必须高于）只用于逐仓（`marginMode=isolated`）仓位**；全仓（`crossed`）仓位的强平价是账户级的共享价格，净多头账户上空单一侧也会报告同一个低于标记价的价格，所以全仓不做方向检查，两条腿都显示这个共享强平价。
  成交记录（`/fills`）里 hedge 的 `side` 是仓位方向、`tradeSide` 为 `open` / `close`、`posMode=hedge_mode`，因此开平仓永远不会歧义；订单同步里 hedge 成交的 `PositionSide` 记为 `LONG` / `SHORT`（one-way 为 `BOTH`），`Side` 记为真实下单方向。
- **逐仓杠杆**：hedge + 逐仓下 `set-leverage` 带或不带 `holdSide` 都会同时设置多空两侧的杠杆。

注意：Bitget Demo 使用正常的合约名（如 `BTCUSDT`），只要请求带 `paptrading: 1` 且使用 Demo Key 即可（已用 `BTCUSDT` 联网验证下单、止盈止损、持仓、成交与取消；见 `trader/bitget_demo_integration_test.go`，需要 Demo Key 环境变量，默认跳过）。正式使用前仍建议先用小额订单确认。

两种模拟盘的区别：官方 Demo 由 Bitget 撮合，更接近真实成交，但需要账号和 Key，且无法离线复现；`bitget_paper` 不依赖任何账号，规则透明可控，但撮合是上面描述的简化模型。
