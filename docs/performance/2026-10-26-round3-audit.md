# 第三轮优化的独立审计

审计对象：`2e60290` 之后工作区里的第三轮改动（10 个文件，+326/−95），以及
`2026-10-26-round3-optimizations.md` 里的全部结论。
审计方式：不采信报告里的数字与论证，逐条重算；对"这次改动是否真的被测试保护"用变异测试判定。

**结论：核心改动是对的，报告的结论方向也都成立。** 没有发现数据错误、内存泄漏或丢失更新。
但有 4 项需要修的问题（其中 2 项是"改动是对的、但没有测试守着它"），以及一批报告措辞与数字不精确。
下面按"已核实成立 / 发现问题 / 残余风险"三段给证据。

## 1. 已独立核实成立

### 1.1 内存足迹：报告数字可复现，且没有系统性偏差

`HeapAlloc` 差值法在 `n=5000` 时实测稳定（40 次采样 489.7–498.3 B，±1%），但它是"存活字节"，
会受 GC 波动影响，也无法区分"分配了"与"还活着"。为了拿到不受噪声支配的数字，另用**精确**的
`TotalAlloc` / `Mallocs` 差值（单调计数；实例在测量窗口内全部保持存活，因此等于常驻足迹），
对基线与当前各跑一遍：

| 场景 | 基线 `2e60290` | 当前 | 变化 | 报告 |
|---|---:|---:|---:|---|
| 非 TTL，1 分片 | 803.5 B | 488.5 B | **−39.2%** | 805.8 → 498.7（−38.1%） |
| TTL 空闲，1 分片 | 921.9 B | 486.7 B | **−47.2%** | 910.4 → 496.0（−45.5%） |
| 容量 0 | 807.1 B | 483.8 B | −40.1% | 799.9 → 494.1 |
| 16 分片（TTL） | 4890.1 B | 3481.9 B | −28.8% | 4758.6 → 3494.4 |
| 每实例对象数 | 10.32 | 6.24 | −4.08 | 10.40 → 6.39 |

基线数字几乎逐位吻合（803.5 vs 805.8），说明两套方法都没有系统性偏差。

Stage-3 门禁（1000 个空闲 TTL 实例 ≤ 0.5 MiB）：**0.880 MiB → 0.452 MiB**，通过，余量 9.6%。
报告写 0.47 MiB（余量 6%），两种口径都通过。

### 1.2 cleaner 唤醒过滤的活性：黑盒验证通过

`rootNeedsWake` 的失败模式不是数据错误，而是**过期条目物理滞留**，所以只能用 `Len()`/`Size()`
观察（`Peek` 对已过期条目本来就返回 miss，看不出来）。只用公开 API 写了 7 个场景，
1/4/64 分片各跑一遍，共 57 项断言，全部通过：

| 场景 | 断言语义 |
|---|---|
| 短 TTL 自然过期 | 无人访问，700 ms 后 `Len()==0` |
| 只延长 root（`extends` 刻意不唤醒） | 只有被延长的 500 个键留下，其余全部回收 |
| **缩短已有条目的 TTL**（root 前移） | 提前到期的 300 条必须被回收（`Len()==1700`） |
| 移除 TTL（出堆） | 400 个永久条目不丢，过期的清空 |
| `Lookup` 命中过期条目 | 前台路径自行回收，`Len()==0` |
| 堆清空后重新填充（12 轮） | 无滞留 |
| 过期期间 `Close` | 返回后 `Stats().Entries==0`，写被拒绝 |

### 1.3 并发 `Close` 修复确实有效

`Stats()` 与 `Lives()` 不因 `closed` 短路，是唯一还能观察"Close 返回后是否仍有内容/仍注册"的公开通道
（`Len()`/`Size()` 在 closed 后直接返回 0，用它们断言会**恒真**）。4 个同时启动的 closer × 300 轮
× 4096 条 × 64 分片：

| | Close 返回时仍有条目 | Close 返回时仍注册 |
|---|---:|---:|
| 基线 `2e60290` | **10 / 1200** | **39 / 1200** |
| 当前 | 0 / 1200 | 0 / 1200 |

当前版本在 `-race` 下同样 0 违规。

### 1.4 报告的数字可以追到原始输出

- `final.txt`（17:30，最后一轮）逐项复现了 §3.1 与 §4 的热路径与 churn 数字：
  `Lookup` −2.7%/−4.4%、`Set` −4.4%/−3.5%、`Get` −2.0%/+0.7%、`Peek` +1.8%/+0.6%、
  `Sharded/Shards64-24` −5.0%、`ShardedPeek/Shards64-24` 0.0%、create+close −38.4%/−43.4%、
  TTL −39.9%、`ShardedSetExpire/Shards64-24` −4.0%。
- `spread2.txt`（17:28）复现了 §3.2 的 `RenewTTL` −4.8%/−12.3%、`Shards1` −3.8%、
  `Shards1-24` −7.5%，以及 `InsertEvict` +1.6%/+2.2%。
- §3.2 的 CPU profile 结论审计时重跑并留档（`cleaner-control.prof` / `cleaner-work.prof`，2s，
  64 分片 24 线程）：基线 `deadlineSnapshot` cum **3.30%**（`0.35s` flat / `1.35s` cum），
  当前 profile 里**整个 cleaner 一条栈帧都没有**（`runCleaner`、`purgeDueBatch`、`deadlineSnapshot`
  均不在 top 800 节点内）。比报告写的"不在 top 60"更强。

### 1.5 策略与并发细节

- `go.mod` 仍为 `go 1.18`；库代码无 `slices`/`maps`/`min`/`max`/`errors.Join`/`atomic.Int*`/
  `sync.OnceFunc`/`strings.CutPrefix`/`rand/v2`/`clear(`/`WaitGroup.Go` 等 1.18 之后的 API；
  `go build -gcflags=-lang=go1.18` 通过。零外部依赖未变。
- 懒创建通道的发布协议正确：`startWorkerNow` 在 `lifeMu` 下写 `wake`/`done`/`stopped`，再用
  release store 置 `workerStarted`，`notifyCleaner` 先 acquire load 再读 `wake`——happens-before 成立。
  两个并发的首次 `SetExpire` 不会重复起 goroutine（`s.started` 在 `lifeMu` 下复核）。
- `close()` 的 `teardown.Add(1)` 在 publish `closed` 之前、`Wait()` 在观察到 `closed` 之后，
  不存在 Add/Wait 交错；`defer Done()` 覆盖所有返回路径。
- worker 循环没有丢唤醒：`due == 0` 只阻塞在 `done`/`wake`，每次 wake 都重新快照，所以 1 槽 channel
  丢掉重复唤醒无害；`removed == 0` 走 `Gosched` 后重来，不会自旋在分片锁上。
- README 的散文与实测一致（约 500 B、"1000 个空闲 TTL ≤ 0.47 MiB"、约 230 ns、唤醒规则）。
- `Clear()` 会通知 worker，所以清空后不会留下指向已消失 deadline 的 timer。

## 2. 发现的问题（均已在本次审计中修掉）

### 2.1 关键：`extends` 的方向判断完全没有测试

`extends := expiring && oldDeadline != 0 && deadline > oldDeadline` 里那个 `deadline > oldDeadline`
是唯一区分"变晚（可以不唤醒）"和"变早（必须唤醒）"的地方。把整条件简化为
`extends := expiring && oldDeadline != 0`（看起来像一次无害的化简），**原有全套测试仍然通过**：

```
$ go test -count=1 ./...        # extends 简化后
ok  github.com/renshan/... memcache  3.086s
```

但行为已经错了：已有条目 TTL 被缩短时不再唤醒，timer 仍指向原来的（更晚的）deadline，
条目会一直物理滞留到那一刻。用 1.2 的黑盒场景跑这个变异版本，`Len()=2000`，期望 1700：

```
FAIL: shrink-root: Len()=2000, want 1700 (early root must be reclaimed)
```

漏检原因很具体：原来的 `TestCleanerDeadlineChanges` 制造"更早的 deadline"用的是**新键**
（`SetExpire("b", 2, 10)`，`b` 之前不存在，`oldDeadline == 0`），新键走不到 `extends` 分支。

**修复**：新增 `TestCleanerShortenedDeadlineRearms`——把**同一个键**的 TTL 从 90s 缩到 10s
（它当前是 heap root），断言 timer 重排到 10s 并真的回收。当前代码通过，上面那个变异版本在
第一个断言就超时。假时钟驱动，无 sleep。

### 2.2 "变晚不唤醒"没有回归测试（本轮的收益点没人守着）

这个优化是**性能性质**而非正确性性质：多唤醒是正确但慢。结果是整个 `wake-always` 变异
（`rootNeedsWake` 恒返回 true，即回到每次堆变化都唤醒）**能通过全套测试**。
也就是说，把本轮 12.3% 的 `RenewTTL` 收益改回去，CI 不会有一点反应。

**修复**：新增 `TestRootNeedsWake`，表驱动钉住谓词本身：
`unchanged` / `later root` 判否，`empty→live` / `emptied` / `earlier root` 判是。
`wake-always` 变异会让它失败。它守不住"五个调用点是否都调了"（那需要另一层测试），
但守住了谓词和它的方向语义。

### 2.3 并发 `Close` 的回归测试检出率不足

修复是对的（见 1.3），但测试的检出率取决于调度是否命中那个微秒级窗口。在**基线代码**上重复跑
`TestConcurrentCloseWaitsForTeardown`（50 轮）：两批各 20 次运行分别漏检 6 次和 1 次，
合起来 40 次里检出 33 次——**约 18% 的机会 bug 还在、测试却是绿的**。

**修复**：轮数 50 → 400（耗时 0.335s）。同样方法在基线上跑：400 轮 **10/10 全部检出**。
注释里写清了为什么不能调小，以及为什么断言必须用 `Stats().Entries` 而不是 `Len()`。
（提醒：这类测试天生是概率性的，"通过"不等于"没问题"，改代码时不要用它当唯一证据。）

### 2.4 32 位平台直接 panic（本轮之前就存在）

`monitor` 的 `count int64` 排在 `sync.Map` 之后。`sync.Map` 在 386 上是 20 字节，`count` 落到偏移 20，
只有 4 字节对齐，而 386 上的 64 位原子操作要求 8 字节对齐：

```
$ GOARCH=386 go test -run TestMonitor ./...
panic: unaligned 64-bit atomic operation
    github.com/ohmountain/memcache.Lives  monitor.go:63
```

不是本轮引入的（基线同样 panic），但这意味着库在 32 位平台上**完全不可用**，
而 `TestIntrusiveLayout` 里那句 `t.Skip("layout is documented for 64-bit targets")` 让人以为只是布局没验证。

**修复**：把 `count` 移到结构体第一个字段（全局/分配对象的第一个字保证 8 字节对齐），并写明原因。
修完 `GOARCH=386 go test ./...` 全套通过。

### 2.5 小问题（一并修掉）

| 问题 | 处理 |
|---|---|
| `rootNeedsWake` 的 `after == 0` 分支是死代码：`rootDeadline()` 堆空返回 0，`after < before` 已经覆盖 `before > 0` 的情形，`before == 0 && after == 0` 又被首行挡住 | 删掉，并在函数注释里写明"不要再加回来"及原因 |
| `shard` 的字段注释写"八个计数器在第三条 cache line"，实际计数器在偏移 96–159，前四个（`hits`/`misses`/`sets`/`deletes`）与 `free`/`size` 共用第二条 | 按实测偏移改写 |
| `TestIdleInstanceFootprint` 的 640 B 预算比它宣称守着的门禁（500 B/实例）还松，625 B 能过测试但过不了门禁 | 收紧到 560 B（实测 497/494），并说明与门禁的关系 |
| `TestIntrusiveLayout` 只钉 `Sizeof == 192`，钉不住设计真正依赖的 64 字节对齐 | 增加 8 个独立 `new(shard)` 的 `% 64 == 0` 断言（注意必须是逐个分配，切片会走另一个 size class） |
| `cleanerRunning` 对"从未起 worker"返回 false，是正确答案但容易被误当成"确实起过 worker" | 补文档说明语义边界，并指出要证明 worker 存在需另行断言 |
| §1 说"本轮唯一的可观察行为变化" | 漏了并发 `Close` 现在会阻塞；已补 |
| `teardown` 记作 +16 B/实例 | 实测 `cacheState` 152 → 160，**净 +8 B**（WaitGroup 12 B 减去删掉的 `sync.Once`）；两处均已改 |
| §5.8 说 `Shards: 1<<62` 会 panic | 不成立：`shardCount` 把 `n` 钳在 `capacity/64`，`Capacity: 1024` 时得到 16 个分片。只有 Capacity 也巨大才会 OOM；已改 |
| §3.3 把变量名写成 `dueRootPossible` | 实际是 `touchesHeap`；已改 |
| §3.3 只报 `InsertEvict` +1.6%/+2.2% | 独立进程组（各 96 采样）是 +5.3%；已改成"1.6%–5.3% 取决于测量设计，不超过 5%" |
| §3.2 说"两个测试的断言强度不变" | 不准确——它们**在基线代码上也会通过**；已改写并说明新增的两个测试才是真正的守卫 |
| §7 的复现步骤只把 `probe_test.go` 放进当前树 | 对照侧没有 probe 就根本没有这些 benchmark，ABBA 会静默少测一半；已补 `cp` 到 `control/` |
| §7 让人跑 `ab.sh` 并引用 `out/control.txt` / `out/work.txt` | 那两份是 17:21 的**回退前**输出（`Shards1-24` 当时是 +21.7%）；报告表格来自 17:28/17:30 的 interleaved 跑法。已在 §3.2 与 §7 写明这个陷阱 |
| §5.1 的 RWMutex 否决数据只有探针源码、没有结果文件 | 无法复核；已在报告中标注并建议否决依据一并留档 |

## 3. 残余风险（未修，需决策）

1. **五个调用点没有端到端覆盖**：`TestRootNeedsWake` 钉住谓词，`TestCleanerShortenedDeadlineRearms`
   钉住"前移"，但 `set`/`lookup`/`delete`/`Size`/`Clear` 里"是否都正确调用了它"仍靠代码走查。
   失效表现是物理滞留而非数据错误。
2. **没有确定性的多分片 cleaner 测试**：仓库的假时钟 fixture 全部固定 1 分片。1.2 的黑盒场景覆盖了
   1/4/64 分片，但它靠真实时间 sleep，没有进仓库。
3. **`Preallocate` 路径没有专门测试**：它现在是唯一的构造期建 map 路径（本轮改成懒建），
   实测无问题但无守卫。
4. **Go 1.18 未实机验证**：本机只有 1.27.2。`-lang=go1.18` 只保证语法与 API 可用，
   size class 与 192→64 字节对齐的行为仍需 CI 的 1.18 job 实测（`TestIntrusiveLayout` 会失败并提示）。
5. **24 线程方差 10–20%**：`Shards1-24` 那一行区间重叠，−7.5% 只能当方向参考。
6. **仓库里没有原始测量数据**：`.perf-lab/` 是 gitignored，克隆后无法复核任何具体数字。
   建议把关键对照的输出（哪怕只是 `spread2.txt` / `final.txt` 两份）提交进 `docs/performance/data/`。

## 4. 复现

```sh
export GOCACHE=$PWD/.perf-lab/gocache

# 精确足迹对照（基线 vs 当前，同一份程序）
ls .perf-lab/footprint-audit .perf-lab/footprint-base

# 公开 API 黑盒活性场景（1/4/64 分片，57 项断言）
cd .perf-lab/ttl-audit && go run .

# 变异测试矩阵（逐条改坏 rootNeedsWake / extends，跑仓库测试看谁被抓住）
python3 .perf-lab/mutmatrix.py

# 并发 Close 检出率（在基线上跑 20 次，数有多少次失败）
cd .perf-lab/newtest/basebox && go test -count=20 -run TestReviewConcurrentCloseDetection ./...

# 32 位
GOARCH=386 go test -count=1 ./...

make check
```
