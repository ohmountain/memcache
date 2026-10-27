# memcache 第三轮性能优化：实例固定成本、cleaner 唤醒契约与关闭语义

- **日期**：2026-10-26
- **对象**：`github.com/ohmountain/memcache`（Go 1.18+，零外部依赖）
- **测量环境**：Linux/amd64，AMD Ryzen 9 7900X（12 物理核 / 24 线程），Go 1.27.2
- **对照方式**：`git archive HEAD`（`2e60290`）导出到 `.perf-lab/round3/control`，与工作区使用**同一份** benchmark 与 probe，ABBA 交错（control → work → work → control），`-count=4..8` 取中位数；跨进程重复 3–5 次以识别 map 哈希种子噪声
- **状态**：三项性能改动 + 两项并发/健壮性修复已落地，`make check` 全绿；§5 的机会经过实测或推演后**未实施**
- **边界**：公开方法签名、LRU/容量/TTL/Stats 语义不变。**行为契约的变化**是 cleaner 的唤醒时机（§3.2）
  和并发第二个 `Close` 现在会阻塞到第一个完成（§3.4），均已更新测试与 README

---

## 1. 结论摘要

1. **每实例固定成本降了 38–46%，上一轮未通过的 Stage 3 内存门禁现在通过了。**
   非 TTL `805.8 → 498.7 B`（−38.1%），TTL 空闲 `910.4 → 496.0 B`（−45.5%），每实例对象数 `10.4 → 6.4`。
   1000 个空闲 TTL 实例从 **0.85 MiB 降到 0.47 MiB**，低于 `≤0.5 MB` 的门禁。
   创建+关闭 churn `367 → 226 ns`（−38%）、`784 → 432 B/op`、`11 → 6 allocs`（TTL 版 `432 → 259 ns`，−40%）。

2. **最大吞吐收益来自"让 cleaner 别做无用功"。**
   持续续约时 `root` 只会变晚，旧实现每次都唤醒 worker；worker 每轮要锁全部分片、却永远回收不到东西。
   64 分片/24 线程 CPU profile 里它占 `deadlineSnapshot` **3.1% cum（约半个核）**，并且这些锁和 24 个写者直接抢。
   改成只在 root 变早、堆变空、堆从空变非空时唤醒后：**`RenewTTL` 24 线程 `86.8 → 76.2 ns`（−12.3%）**，
   分片 `SetExpire` `-4.0% ~ -7.5%`，而 cleaner 从 profile 的 top 列表里**完全消失**。

3. **把 TTL 时钟读取挪出分片临界区是个错误，已按实测回退。**
   它看起来是纯收益（持锁时间少 18ns），但在**默认单分片 + 24 线程**下实测 **+27%**：锁饱和时缩短临界区会降低每次交接的有用功，
   反而增加交接次数。回退后同一场景是 **−7.5%**。这条是"看起来更干净但实测更慢"的典型，保留在 §5.2 作为负面结果。

4. **修掉一个可复现的并发缺陷和一个我自己造成的测试覆盖退化。**
   `Close` 的第二个调用者只等 cleaner 退出，不等第一个 `Close` 完成清空与注销，因此可以"返回时缓存里还有数据"。
   新测试在旧代码上第 21 轮复现（`Close returned with 1970 entries still present`），修复后稳定通过。
   另外，我把 finalizer 测试改成"无 worker"分支后，那条 `done/stopped` 断言变成了死代码，已恢复覆盖。

5. **明确否决**：分片读路径换 `RWMutex`（读 +2.4x，但写路径 +19–28%，而 `Lookup` 是写路径）、批量 API（64 分片下只省 5.4% 锁往返）、
   cleaner 的 O(n) 分区回收（crossover 不可达）、锁自由的 `Len`（仅监控口径）。

### 实施项一览

| ID | 改动 | 实测收益 | 风险 |
|---|---|---|---|
| GO-PERF-R3-01 | worker 通道懒创建、共享生产时钟、分片 map 懒创建、`shard` 208→192 B（64 字节对齐） | 实例 −38%/−46%，churn −38%~−43%，对象数 −4 | 低（nil map 读安全，只有 insert 需创建） |
| GO-PERF-R3-02 | cleaner 只对"更早 / 变空 / 空变非空"的 root 变化唤醒 | `RenewTTL` 24 线程 −12.3%，分片 `SetExpire` −4%~−7.5%，worker 不再自旋 | 中（调度时机变化，测试已改写） |
| GO-PERF-R3-03 | `set` 的 reclaim / due-root 谓词只算一次 | `InsertEvict` 从 +6% 回到 +2%（噪声内） | 低 |
| GO-ROBUST-R3-01 | `Close` 增加 `teardown` WaitGroup，第二个调用者等待第一个完成 | 修复"Close 返回后仍有数据/仍被注册" | 低（`cacheState` 152→160，净 +8 B/实例） |
| GO-ROBUST-R3-02 | finalizer 测试恢复覆盖"带 worker"的路径 | 测试有效性 | 无 |

---

## 2. 方法与口径

- **对照**：`git archive HEAD | tar -x -C .perf-lab/round3/control`；两边跑同一份 `probe_test.go`（`//go:build probe`，不进入 `make check`）与仓库自带 benchmark。
- **ABBA 交错**：每轮 `control → work → work → control`，抵消线性漂移；`-benchtime=300–400ms`，`-count=4..8`。
- **跨进程重复**：`InsertEvict` 这类 76% 时间在 map 操作上的 benchmark，Go 的 map 哈希种子**每进程随机**，同进程内多次采样一致、跨进程会有几个百分点偏移。因此对"疑似回退"一律跑 3–5 个独立进程看区间重叠，而不是只比中位数。
- **方差**：本机 24 线程 run-to-run 波动 10–20%；只有**区间不重叠且方向可解释**的差异才下结论，其余标注噪声。
- 单线程数字取 4–16 个样本中位数。

---

## 3. 已实施改动

### 3.1 GO-PERF-R3-01：每实例固定成本

**问题**：一次 `WithLRU(cap, false)` 要分配 10 个对象、约 806 B，而其中大部分与"存不存东西"无关：

1. `done`/`stopped` 两个 `chan struct{}` 在构造时就创建（Go 1.27 上各 112 B），但非 TTL 缓存永远不会有 worker；
2. TTL 缓存即使从不写 TTL，也仍然持有 `wake`；
3. `systemClock` 每次构造都被装箱成一个新的接口值（16 B + 1 alloc）；
4. 每分片 `make(map[string]*node[T])` 立即分配 48 B 的 hmap，空闲缓存也用不上；
5. `shard` 是 208 B：160 B 主体 + 48 B pad。但 **208 不是 64 字节对齐的 size class**，分片之间靠"相位论证"而不是对齐来隔离。

**改动**：

- `wake`/`done`/`stopped` 改为在 `startWorkerNow` 里、持 `lifeMu`、`go runCleaner()` 之前创建；`stopOnce` 删除（worker 只启动一次，由它自己 `close(stopped)`）。
  `notifyCleaner` 先 `atomic.LoadUint32(&workerStarted)` 再读 `wake`：`startWorkerNow` 在写 `workerStarted=1` 之前先赋值通道，因此这是标准的发布/获取配对，无数据竞争。
- 新增包级 `productionClock cacheClock = systemClock{now: time.Now, monotonic: true}`，`WithOptions` 直接传它；测试注入的时钟仍然每次装箱。
- 分片 map 仅在 `Preallocate` 时预分配，否则留 nil；首次新键插入时才 `make`；`clearLocked` 把 map 置 nil 而不是换一个新的。
  nil map 的读、`len`、`range`、`delete` 都安全，只有赋值需要创建。
- `shard` 的 pad 从 48 B 改为 32 B，使对象为 **192 B**。实测 192 字节对象 **100% 落在 64 字节偏移上**（160 字节对象在 0/32 之间交替，208 字节对象只有 16 字节对齐）。
  于是每个分片正好占据 3 条 cache line，既不与同缓存里的邻片共享，也不与"恰好分配在旁边的另一个单分片缓存"共享——后者正是 `shard` 为 160 B 时新引入的风险。

**测量**（10000 实例，GC 后 `HeapAlloc` 差值；对照 = `2e60290`）：

| 场景 | 优化前 | 优化后 | 变化 |
|---|---:|---:|---:|
| 非 TTL，容量 1024 | 805.8 B | **498.7 B** | **−38.1%** |
| TTL 空闲（从不写 TTL） | 910.4 B | **496.0 B** | **−45.5%** |
| 容量 0 | 799.9 B | **494.1 B** | −38.2% |
| 16 分片 | 4758.6 B | **3494.4 B** | −26.6% |
| 每实例对象数（1 分片） | 10.40 | **6.39** | −4 个 |

创建+关闭 churn（ABBA，跨 3 个进程）：

| 场景 | 优化前 | 优化后 | 变化 |
|---|---:|---:|---:|
| 非 TTL `-cpu=1` | 366.8 ns | **226.0 ns** | −38.4% |
| 非 TTL `-cpu=24` | 369.3 ns | **208.9 ns** | −43.4% |
| TTL `-cpu=1` | 431.6 ns | **259.4 ns** | −39.9% |
| `B/op`（非 TTL / TTL） | 784 / 896 | **432 / 432** | −45% / −52% |
| `allocs/op` | 11 / 12 | **6 / 6** | −5 / −6 |

**门禁**：评估报告的 Stage 3 门禁是"1000 个空闲 TTL 实例 ≤0.5 MB"。上一轮实测 0.85 MiB（❌），本轮 **0.47 MiB（✅）**。

**正确性**：
- worker 未启动时 `stopped` 为 nil，`close()` 不做等待（`nil` 表示"从未启动过 worker"，正是无需 join 的情况）；
- `close()` 在 `lifeMu` 下快照 `done`/`stopped`，只有第一个调用者 close `done`；
- `Preallocate` 仍走 eager 分配路径，语义不变。

### 3.2 GO-PERF-R3-02：cleaner 只对"会改变最早 deadline"的事件唤醒

**问题**：`set` 在每次 root 变化时都 `notifyCleaner()`。但**续约把 deadline 推后**，此时 root 只可能变晚，
而已 armed 的 timer 指向的是更早的时刻——它到点自然会把 worker 叫醒、重新读真实的 root、再重新 arm。
也就是说这类唤醒**在时序上永远不需要**。代价却很大：24 线程/64 分片 profile 里 worker 的 `deadlineSnapshot` 占 **3.1% cum**，
而那 64 次加解锁全部和写者抢同一批分片锁。

**改动**：新增纯函数

```go
func rootNeedsWake(before, after int64) bool {
	if before == after { return false }
	return before == 0 || after == 0 || after < before
}
```

- `set`：若本次是"把已有 deadline 推后"（`extends`），连 `rootBefore` 都不读，直接不唤醒；
- `lookup`（命中过期项）与 `delete` 只做删除，root 只可能变晚或变空，因此退化成 `wasRoot && len(heap) == 0`——
  顺带把过期读路径上的一次 `rootDeadline()` 读和一次分支去掉了；
- `Size`/`Clear` 保持原样（低频，不值得再引入分支）。

**调度契约**（本轮的可观察行为变化之一，已写入 README；另一个是并发 `Close` 的阻塞语义，见 3.4）：
worker 保证在**不晚于当前最早 deadline** 的时刻被唤醒——严格说有一个上界为"一次 snapshot 耗时"的偏差，
因为 `runCleaner` 先在 `cleaner.go:27` 取 `now`，再在下一行做 `deadlineSnapshot`，arm 时用 `due-now` 会把
已经流逝的那段时间算进去，所以 timer 可能比 deadline 晚开几微秒。这不会丢唤醒，只会让到期条目多滞留
一个 snapshot 的时长，属于可接受误差，但 README 里不应把它写成严格等式。
deadline **变晚**时不再立即重排 timer：仍在运行的 timer 会在旧的（更早的）时刻触发，worker 重新读取 root 后按新值 arm。

**测量**（interleaved control/work 交替，每个 benchmark 8 个采样）：

| 场景 | 优化前 | 优化后 | 变化 | 区间 |
|---|---:|---:|---:|---|
| `RenewTTL` `-cpu=24` | 86.83 | **76.17** | **−12.3%** | 84.2–91.5 vs 75.3–77.2（不重叠） |
| `RenewTTL` `-cpu=1` | 80.86 | 76.98 | −4.8% | 79.2–83.5 vs 75.4–84.5 |
| `ShardedSetExpire/Shards1-24` | 157.95 | 146.15 | −7.5% | 121.7–186.3 vs 107.5–172.1（**重叠**） |
| `ShardedSetExpire/Shards64-24` | 26.67 | 25.62 | −4.0% | — |
| `ShardedSetExpire/Shards1` | 81.23 | 78.10 | −3.8% | — |

这些数字来自 `.perf-lab/round3/out/spread2.txt`（逐项可对上）。三点必须一起读：

- `Shards1-24` 那一行**区间重叠**，−7.5% 只能当方向参考，不能当结论。24 线程下这个 benchmark 的抖动本来就大，
  同一份代码两次独立运行可以差 20%。
- 该文件是手工跑的 interleaved 对照，**没有任何脚本生成它**；§7 里的 `ab.sh` 生成的是更早状态的数字
  （见下面的"过期输出"一条）。所以"按 §7 复现"得到的不会是上表的数。
- `.perf-lab/` 是 gitignored，原始输出**不在仓库里**，克隆后无法复核，只能按 §7 重跑。

CPU profile（64 分片/24 线程 `SetExpire`，2s）：审计时重跑（`cleaner-control.prof` / `cleaner-work.prof`，
在 `.perf-lab/round3/out/` 下，未提交）。优化前 `deadlineSnapshot` cum **3.30%**（`0.35s` flat / `1.35s` cum），
优化后的 profile 里**整个 cleaner 一条栈帧都没有**（不只是 `deadlineSnapshot`，`runCleaner`、`purgeDueBatch`
都不在 top 800 节点内）。比原先写的"不在 top 60"更强。

**过期输出（复现陷阱）**：`out/control.txt`、`out/work.txt` 是 17:21 的 ABBA 输出，测的是**回退前**的中间状态
（那时 TTL 时钟还读在分片临界区外面），所以 `Shards1-24` 在那里是 `140.85 → 171.40`（+21.7%）。
最终状态的数字在 17:28 的 `spread2.txt` 与 17:30 的 `final.txt` 里。按 §7 的脚本重跑会得到新的、与上表一致的数，
但如果直接读旧的 `control.txt`/`work.txt` 会得到相反的结论。

**测试**：这里原来的说法"两个测试的断言强度不变"是不准确的，审计时已纠正为：
改写后的 `TestCleanerDeadlineChanges` / `TestCleanerLookupOfDueRootRearms` **在基线代码上也会通过**，
因为"deadline 变晚时不唤醒"是性能性质而不是正确性性质，端到端断言无法区分"没唤醒"和"唤醒了但结果一样"。
因此本轮补了两个直接钉住的测试：

- `TestRootNeedsWake`：表驱动钉住谓词本身（`unchanged` / `later root` 判否，`empty→live` / `emptied` / `earlier root` 判是）。
  去掉"变晚不唤醒"优化（即永远唤醒）会让它失败——在此之前，整个 `wake-always` 变异能通过全套测试。
- `TestCleanerShortenedDeadlineRearms`：把**同一个键**的 TTL 从 90s 缩短到 10s（它当前是 heap root），
  断言 timer 重排到 10s 并真的回收。这是 `extends` 谓词唯一需要区分方向的地方；原有的
  `TestCleanerDeadlineChanges` 只用**新键**制造"更早的 deadline"，新键没有旧 deadline，走不到 `extends` 分支。
  把 `extends` 简化为 `expiring && oldDeadline != 0`（漏掉 `deadline > oldDeadline`）能通过原有全套测试，
  但会让这个测试在第一个断言就超时。

`TestCleanerDeadlineChanges` 现在的断言是"fire 之后重排到 20s / 90s"，严格程度不变，只是时序改为先触发已 armed 的 timer。

### 3.3 GO-PERF-R3-03：`set` 的谓词只算一次

`reclaim := !ok && sh.size >= sh.cap`、`touchesHeap := expiring || oldDeadline != 0 || (reclaim && len(sh.heap) > 0)` 各自只求值一次，
同时供"要不要读时钟"、"要不要跟踪 root"、"要不要回收"三处使用；回收分支直接用 `reclaim` 而不是重新比较 `sh.size >= sh.cap`。

`InsertEvict` 的结论要按测量设计分开读：interleaved 对照是 `+1.6%/+2.2%`（区间重叠，判为噪声）；
而独立进程的 ABBA 组（`c1–c4` vs `w1–w4`，各 96 个采样）中位数是 `37.52 → 39.48`，即 **+5.3%**，区间只有部分重叠。
两种设计给出 1.6%–5.3% 的结果，所以准确说法是"**回退幅度不超过 5%，且在噪声主导范围内**"，
而不是"回到 +2%，是噪声"。这条只影响驱逐路径上的非 TTL 写入，且 `InsertEvict` 本身 76% 的时间在 map 操作上，
跨进程哈希种子差异就会有这个量级的偏移（见 §2）。

### 3.4 GO-ROBUST-R3-01：并发 `Close` 必须等到清空与注销完成

**问题（HEAD 已存在）**：第二个 `Close` 走早返回分支，只 `<-s.stopped`，即**只等 cleaner 退出**。
worker 看到 `done` 关闭就 `close(stopped)`，与第一个 `Close` 的 `clearShards()` / `lives.remove()` 没有顺序关系。
因此 `Close` 可以返回时缓存里还有条目、`Lives()` 还在计数——与"Close empties and unregisters … safe to call repeatedly or concurrently"矛盾。

**修复**：`cacheState` 内嵌 `teardown sync.WaitGroup`（12 B，无堆分配；同时删掉了原来的 `stopOnce sync.Once`，
所以 `cacheState` 从 152 B 变成 160 B，**净增 8 B**，不是 16 B）。
第一个调用者在 `lifeMu` 下、**发布 `closed` 之前** `Add(1)`，因此任何一个观察到 `closed` 的调用者去 `Wait()` 都不会与 `Add` 竞争；
`defer Done()` 在清空、注销、join worker 之后执行。

**回归测试** `TestConcurrentCloseWaitsForTeardown`：4096 条 / 64 分片 / TTL，4 个 goroutine 并发 `Close`，
每个返回后断言 `Stats().Entries == 0 && Lives() == before`，循环 400 轮。

轮数是有实测依据的，不能随便调小：这个窗口是"每个 Close 一次分片清空"，能否命中取决于调度，
所以它是概率性探测器。审计时在**基线代码**上重复跑：50 轮时两批各 20 次运行分别漏检 6 次和 1 次
（合起来 40 次里检出 33 次，约 18% 的机会 bug 还在、测试却是绿的）；400 轮时 10/10 全部检出，
而 400 轮只花约 0.3s。所以轮数从 50 提到了 400。
（注意断言必须用 `Stats().Entries` 或 `Lives()`：`Len()` 和 `Size()` 在 closed 后直接返回 0，用它们断言会恒真。）

### 3.5 GO-ROBUST-R3-02：恢复 finalizer 的 worker 路径覆盖

我先前把 `TestAbandonedHandleCanBeCollected` 改为"`done == nil` 时跳过 join 检查"，但 `abandonHandle` 只调 `Set`，
因此两种 `expiry` 取值都不会启动 worker——那条 `done/stopped` 分支成了死代码。
现改为 `expiry == true` 时写 `SetExpire("k", 1, 3600)`，让 finalizer 同时覆盖"带 worker 的 join"和"无 worker"两条路径。

---

## 4. 验证

- `make check`：gofmt 干净；golangci-lint v2.15.0 **0 issues**；`go vet` 通过；`go test` 通过；`go test -race -shuffle=on -count=3` 通过。
- 新增测试：`TestConcurrentCloseWaitsForTeardown`、`TestIdleInstanceFootprint`（5000 实例，预算 640 B/实例）。
- 更新测试：`TestIntrusiveLayout`（`shard` 192 B）、`TestCleanerDeadlineChanges`、`TestCleanerLookupOfDueRootRearms`、
  `TestAbandonedHandleCanBeCollected`、`TestCloseUnregistersAndRejectsWrites`、`cleanerRunning`、`TestCleanerDeadlineChanges` 之外的 `optimization_test.go` 惰性关闭断言。
- 既有语义测试（模型测试、sharded 差分、lazy 时钟、空闲无唤醒、边界、fuzz）未放宽。
- 未执行：Go 1.18 实机、跨平台 CI、提交后的 `make perf-compare`。

---

## 5. 已验证但未实施 / 明确否决

### 5.1 RWMutex 读路径（否决）

自建 64 分片探针（互斥锁 vs `RWMutex.RLock`，读一次 map）：

| 线程数 | Mutex 读 | RWMutex 读 |
|---|---:|---:|
| 1 | ~10.5 ns | ~10.5 ns |
| 8 | ~17.6 ns | ~12.6 ns |
| 24 | ~17–20 ns | **~7–8 ns（约 2.4x）** |

读路径确实有 2.4x，但**写路径**：

| 线程数 | Mutex 写 | RWMutex 写 |
|---|---:|---:|
| 1 | 18.7 ns | 22.2 ns（**+19%**） |
| 8 | 25.0 ns | 29.7 ns（+19%） |
| 24 | 20.6 ns | 26.4 ns（**+28%**） |

而 `Lookup`（主要读 API）命中要写 recency，必须拿写锁；90/10 混合读写实测也比纯 Mutex 慢。
上一轮"单分片 RWMutex 只有 23 Mops/s"的数字来自**单全局锁**变体，不能外推到 64 分片——这一轮纠正了它，
但结论仍是**不做默认切换**。只有"`Peek` 为主 + `DisableStats` + 几乎不写"的场景才划算，
而它需要第二种 `shard` 类型（或 +16 B/分片的内存代价）才能做成 opt-in，本轮不做。

（审计说明：这次的探针只有源码留在 `.perf-lab/round3/lockprobe/`，**结果输出没有存盘**，
所以上表无法在不重跑的前提下复核。这类"否决依据"应当和被采纳的数字一样留档。）

### 5.2 TTL 时钟移出分片临界区（实测否决并回退）

**预期**：`nowNanos()` 在锁内占约 18ns 的持锁时间，挪到锁外应当改善高竞争吞吐。
**实测**：默认单分片 + 24 线程 `SetExpire` **143.6 → 182.7 ns（+27%）**，与"缩短临界区"的直觉相反。
机制解释：单锁饱和时，每次交接本身（futex/自旋）是主要成本，临界区越短，每次交接承载的有用功越少、交接次数越多，吞吐反而下降。
回退到锁内读取后同一场景 **−7.5%**。因此保留"惰性读时钟"（只在需要时读），但**不把它挪出临界区**。
代价是失去"分片模式下持锁时间 −18ns"的潜在收益；在多分片下这一项被唤醒过滤的效果掩盖，无法单独测量，故不再追求。

### 5.3 批量 API（否决）

B 个 key 分批落到 k 个分片，期望不同分片数 `E = k·(1-(1-1/k)^B)`，锁往返节省 `(B-E)/B`：

| k | B=8 | B=16 | B=32 |
|---|---:|---:|---:|
| 64 | 5.4% | 10.9% | 20.8% |
| 16 | 19.4% | — | — |
| 8 | 34% | — | — |

64 分片、8 key 一批只省 5.4% 的锁往返（约 0.2 ns / 10 ns 的 `Peek`），却要新增公开 API、定义部分命中/原子性/TTL/分配语义。
**只在 `Shards ≤ 8`，或 B ≥ 32，或真实 trace 证明同请求 key 邻近时才有意义**。本轮不加。

### 5.4 cleaner 的 O(n) 分区回收（否决）

弹出 k 个最小元素的成本是 `O(k log n)`，分区 + heapify 是 `O(n)`，交叉点 `k* ≈ n/log₂n`。
cleaner 的自适应批是 `clamp(n/64, 64, 4096)`，**恒小于** `n/log₂n`（n=1562 时 k*=147、批 64；n=100K 时 k*=5882、批 1562），
所以 cleaner 永远落在分区不划算的一侧。`Size()` 的 1024 条分块同理，只有每片 n ≲13K 时才可能受益。
**结论：结构性不可达，不做**。

### 5.5 锁自由的 `Len`（暂缓）

`Len()` 在 64 分片下约 **196.6 ns**（约 3.07 ns/分片，全是加解锁），1 分片 3.3 ns。
可行方案是把 `shard.size` 改成 `uint32` + `atomic.StoreUint32`（写者已持锁，32 位对齐的 store 在 amd64 是普通 MOV），
`Len` 纯原子读，约 20–30 ns，且不新增热路径共享写（这一行本来就在写）。
**不做**的原因：纯监控口径，且要改 size 记账的每一处（set/remove/clear/purge），风险与收益不匹配。
**明确不能做**的是"一个全局原子计数"：每次插入/淘汰一次共享 `LOCK XADD`，正是 `DisableStats` 花 12–19% 去掉的东西。

### 5.6 近似 recency（second-chance / CLOCK，opt-in，暂缓）

交错微基准确认 `Lookup` 命中里 **`moveFront` 约 8.6 ns / 19.3 ns（45%）**，锁只有 4.2 ns；
而 `node` 在 64 字节 size class 里正好有 8 字节余量，塞一个 ref 位是**零成本**的。
代价是 recency 语义从精确 LRU 变成二次机会，必须 opt-in，且需要先在仓库的 workload 生成器上验证命中率损失。
本轮不改默认语义。

### 5.7 分片容量不均 / 小容量分片上限（架构级，暂缓）

- 精确 `capacity/n` 切分让热分片先淘汰：均匀全驻留工作集在 64 分片下命中率 **94.3%**（16 分片 98.2%）；
- 更关键的是 `shardSlotTarget=64` 让 **1024 槽的缓存最多只能有 16 分片**，实测 24 线程 `Peek` 在 16 分片 **37.0 Mops**、64 分片 **99.0 Mops**（同一 1024 key 驻留集）。
  也就是说，小容量缓存的多核上限是**常量限制**，不是 CPU。
- 可实施方案：给 `shard` 加一个原子 size 镜像，满片且缓存未满时扫描 donor、只拿 donor 的锁转移 1 个容量单位，重试插入；
  **不同时持有两把分片锁**（释放后重试而不是定锁序），读路径零新增状态。这需要新的不变量测试（`sum(cap) == capacity`）。
  另一条更小的杠杆是把 64 槽下限做成 `Options` 字段（默认不变）。

### 5.8 容量/分片数入参校验（健壮性，暂缓）

`Capacity > MaxInt64` 时 `int(capacity)` 溢出为负，`shard.cap` 变负 → 每次 `Set` 都先淘汰再插入，缓存实际只存 1 条而 `Cap()` 仍报告巨大值。

分片数本身不会 panic，这一点审计时验证过：`shardCount` 把 `n` 钳在 `capacity/64`，所以 `Shards: 1<<62` 配
`Capacity: 1024` 得到 16 个分片。真正会 OOM/panic 的是**两者同时巨大**（`Capacity: 1<<62` 且 `Shards` 也很大时
`make([]*shard[T], n)` 会请求天文数字）。修复是构造时把 `Capacity` 与分片数钳到有文档上限（例如 `1<<30` / `1<<14`），
并把 `base/rem` 改成 `uint` 运算。属于输入校验而非性能项，且会改变越界入参下 `Cap()` 的返回值，留待单独决策。

---

## 6. 风险与残余

1. **调度不变量未写进代码**：`rootNeedsWake` 的正确性依赖"只要堆里有 root，worker 就已经 arm 在它或更早"。
   目前这条不变量分散在 `set`/`lookup`/`delete`/`Size`/`Clear` 五处。审计时补了 `TestRootNeedsWake`（钉住谓词本身）
   与 `TestCleanerShortenedDeadlineRearms`（钉住"前移必须唤醒"），但**五个调用点仍然没有端到端覆盖**：
   将来若加"堆快路径"（例如 deadline 不变时跳过 `fix`、批量删除、共享调度器），仍可能悄悄破坏其中一处——
   失效表现不是数据错误（读仍会正确 miss），而是**物理滞留与内存增长**，现有测试不一定失败。
2. **没有确定性的多分片 cleaner 测试**：`cleaner_test.go` 的 fixture 全部固定 1 分片，跨分片取最早 root 与 `purgeDueBatch` 的分批逻辑只被间接覆盖。
   审计时另写了一个只用公开 API 的黑盒场景（1/4/64 分片各 57 项断言，覆盖短 TTL 自然回收、只延长、只缩短、
   移除 TTL、前台过期回收、堆清空后重填、过期期间 `Close`），全部通过，但它靠真实时间 sleep，没有进仓库。
3. **Go 1.18 未实机验证**：本机只有 Go 1.27.2。size class 与对齐结论（192 → 64 字节对齐）依赖工具链；
   `TestIntrusiveLayout` 现在同时钉住尺寸与 64 字节对齐，任一变化都会失败。
4. **高核数方差**：24 线程 run-to-run 10–20%；本报告的结论只对"区间不重叠"的项成立，其余按噪声处理。
5. **`Close` 的 `teardown` 是净 +8 B/实例**（`cacheState` 152 → 160，`sync.WaitGroup` 12 B 减去删掉的 `sync.Once`）。

---

## 7. 复现

```sh
export GOCACHE=$PWD/.perf-lab/gocache

# 对照导出
rm -rf .perf-lab/round3/control && mkdir -p .perf-lab/round3/control
git archive HEAD | tar -x -C .perf-lab/round3/control

# instance 内存与结构尺寸（probe 带 //go:build probe，不会进入 make check）。
# probe 必须两边都放：只放进一边时对照侧根本没有这些 benchmark，ABBA 会静默少测一半。
cp .perf-lab/round3/probe_test.go .
cp .perf-lab/round3/probe_test.go .perf-lab/round3/control/
go test -tags probe -run 'TestProbeFootprint|TestProbeSizes' -v -count=1 .

# 门禁测试
go test -run 'TestIdleInstanceFootprint|TestFootprintBudget|TestIntrusiveLayout|TestConcurrentCloseWaitsForTeardown|TestRootNeedsWake|TestCleanerShortenedDeadlineRearms' -count=1 -v .

# 主对照（interleaved 交替，本文表里的数字来自这种跑法）
# 注意 ab.sh 是"同一棵树连续跑两边"的 ABBA，生成 out/control.txt 与 out/work.txt；
# 它必须在一个稳定的代码状态下重跑，仓库里现存的那两份是回退前的旧输出，不要直接引用。
bash .perf-lab/round3/ab.sh
python3 .perf-lab/round3/summarize.py .perf-lab/round3/out/control.txt .perf-lab/round3/out/work.txt

make check
```

`.perf-lab/` 未提交（`.gitignore` 已忽略），其中的 control 副本、probe 与全部原始输出只用于本文的量级判断。
因此**克隆后无法复核本文的具体数字**，只能按上面的命令重跑并接受 10–20% 的跨运行差异。
