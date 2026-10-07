# Realm L2 出口节点构建指南（家宽住宅出口选型 / 软路由一体化 / 容量实测）

> 状态：🆕 **构建指南 + 选型讨论（持续完善，含待实测项）**。沉淀关于"如何构建一个高性能 realm L2 住宅出口"的讨论与事实。
> 日期：2026-06-24
> 读者：建/选 realm 出口的人 + 运维。
> 定位：[[REALM_END_TO_END_ARCHITECTURE]] §0.0.1 把"出口容量由硬件决定"作为事实记下，本文是它点名的那份**独立硬件选型与构建指南**的落地。回答：**一台 realm L2 出口该怎么建、瓶颈在哪、能扛多少、怎么实测验证。**
> 配套：容量模型见 [[FLEET_MANAGER_DESIGN]] §5.5；部署/排障见 [[../ops/REALM_EGRESS_NODE_OPS_GUIDE]]。

---

## 0. 一句话结论

realm L2 出口的容量**没有协议硬上限，完全由硬件 + 家宽决定**（[hysteria2 Performance](https://v2.hysteria.network/docs/advanced/Performance/) / [sing-box hysteria2 inbound](https://sing-box.sagernet.org/configuration/inbound/hysteria2/)，2026-06 查证）。瓶颈优先级**随家宽规格漂移**：

- **低上行家宽（国内几十 Mbps）** → 瓶颈 = **上行带宽**，CPU/内存富余。
- **高对称家宽（如 Optimum 2.5G 对称）** → 上行不再是瓶颈，瓶颈转移到 **CPU（QUIC 用户态加解密）**。

所以"建什么样的出口"要**按你的家宽规格倒推该配什么硬件**，不能套固定配方。

---

## 1. 瓶颈四维（按家宽规格排序会变）

| 瓶颈 | 量化方式 | 低上行家宽 | 高对称家宽 |
|---|---|---|---|
| **① 上行带宽** | 可服务满速用户数 ≈ 上行 ÷ 人均码率 | ★第一瓶颈 | 不再是瓶颈 |
| **② CPU（QUIC 用户态加解密）** | 喂满目标带宽所需算力；QUIC 比内核态 TCP 更吃 CPU | 富余 | ★**新第一瓶颈** |
| **③ 内存（每连接接收窗口）** | stream 8MB + conn 20MB ≈ 峰值 28MB/连接；实际均摊 2–4MB | 富余 | 大并发时才轮到 |
| **④ 家用 NAT 会话表** | 路由器/光猫 conntrack 表项上限 | 低端光猫先爆 | 取决于路由形态（见 §3） |

> 协议层那个 `max_concurrent_streams` 限的是**单连接内并发 stream**，可配置，**不是用户/连接总数限制**，永远不是瓶颈。

---

## 2. 上行带宽：第一要务是实测，不是看套餐标称

**"千兆光纤" ≠ 千兆上行。** 容量按**上行**算，所以建出口前第一步永远是：

```
speedtest（或 speedtest-cli）实测 upload → 拿到真实上行 Mbps
容量上限（满速用户数）≈ 真实上行 ÷ 人均码率（看视频约 2Mbps，4K 约 15–25Mbps）
```

| 家宽 | 真实上行 | 人均 2Mbps 满速容量 |
|---|---|---|
| 国内"千兆"（非对称） | 30–100 Mbps | ~15–50 人 |
| **Optimum 2.5G 对称** | **~2500 Mbps** | **~1000+ 人**（届时 CPU 先到顶，非带宽） |

---

## 3. ★Optimum 专项（用户实际宽带：计划 2.5G 对称）

用户当前用 **美国 Optimum（Altice）宽带，计划上 2.5G 对称**。美国运营商与国内差异巨大，对本方案**整体有利**。以下为查证事实（2026-06）。

### 3.1 不用 PPPoE，走 DHCP（比国内简单）

Optimum 等美国光纤/有线系运营商**用 DHCP，不是 PPPoE**。含义：
- **无需向运营商要拨号账号密码**（国内 PPPoE 的最大麻烦在此处不存在）。
- 桥接后，你的路由器 / 出口机直接 **DHCP 拿公网 IP**。一体化软路由方案里，出口 Linux 用 `dhclient`/`systemd-networkd` 在 WAN 口拿地址即可，**不需要配 PPPoE**。

### 3.2 网关桥接：客服电话即可，但有型号坑

Optimum 网关（ONT+路由一体）改桥接**打客服电话就行，无需上门**（[asheroto 指南](https://asheroto.medium.com/using-your-own-router-firewall-with-optimum-suddenlink-and-how-to-disable-wi-fi-too-e705862497de)）。但**型号决定能不能稳定桥接**：

| 网关型号 | 桥接支持 | 坑 |
|---|---|---|
| GR140DG / GR140D / UBC1326 | ✅ 客服电话可桥接 | 较干净 |
| **GR240BG**（2.5G/多 Gig 常配此型） | ⚠️ 可桥接，**但重启后会掉回 IP_Routed 路由模式** | ★对 7×24 出口是真实风险（[DSLReports](https://www.dslreports.com/forum/r32573603-FiberGateway-GR240BG)）——停电/重启后桥接失效=出口暴露在 double NAT 下，打洞退化 |

> **★上 2.5G 时务必先确认运营商给的网关型号**。若是 GR240BG，要么接受"重启需重设桥接"的运维负担（配 UPS 减少重启 / 监控桥接状态），要么争取换型号 / 自购认证设备。

### 3.3 退路：不桥接 = double NAT（打洞打折，不推荐）

若网关无法桥接，出口机挂在网关下面走 **double NAT**——能跑但**打洞要穿两层 NAT**，cone NAT 行为更难保证，打洞成功率和容量都打折。**realm 强依赖打洞，所以桥接是首选，double NAT 是下策。**

---

## 4. ★路由形态选型：独立路由器 vs 软路由一体化进出口 Linux

桥接之后，谁来拨号/拿 IP + 做 NAT？两种形态，**按"出口部署在谁家"选**：

### 4.1 形态 A：独立高端路由器 / 软路由 + 出口机在其后

```
光猫(桥接) → 独立路由器(DHCP 拿公网/NAT) → 出口机(otun-realm-agent + sing-box)
```
- **优点：故障隔离**。软路由/路由器与出口机分离，出口机挂了不影响这个家正常上网，**路由挂了也只是这一摊**。
- **适用：部署在别人家的住宅出口**——避免"出口机/软路由出问题 → 房主断网 → 投诉/被拔电"。
- 缺点：多一跳转发；路由器若低端，NAT 表/小包转发会成为瓶颈（④）。

### 4.2 形态 B：★软路由一体化进出口 Linux（光猫桥接 → 出口机直接拿 IP + 跑 sing-box）

```
光猫(桥接) → 出口机 Linux：WAN 口 DHCP 拿公网 IP + nf_conntrack NAT + sing-box 一体
```
- **优点**：
  - **消灭中间路由 NAT 瓶颈**：sing-box 进程所在机直接持有公网接口，NAT 用 Linux 内核 `nf_conntrack`（可调**百万级**会话，远超家用路由器）。
  - **少一跳转发**，延迟/抖动更低。
  - **打洞更干净**：直接拿公网映射，STUN 少穿一层 NAT，cone NAT 行为更可控。
  - 一台机搞定，省成本/运维。
- **代价 / 前提**：
  - **CPU 双负载**：拨号/NAT 转发 + QUIC 加解密两份压一台机 → **必须高性能 CPU**（正是 2.5G 对称场景的核心要求）。
  - **故障域耦合**：软路由挂 = **整条家宽断网**（不只是 VPN）。
  - 需要内核调优（§6）才能榨满高带宽，不是装上即最优。
- **适用：自己掌控的高性能出口**（自己的机器/可信场地）。Optimum 2.5G 对称 + 高性能服务器，**正是形态 B 的理想场景**。

### 4.3 选型口诀

> **自己的高性能机 → 形态 B（一体化软路由）榨满对称大带宽；别人家的住宅出口 → 形态 A（独立路由器+桥接）保故障隔离。**

---

## 5. 硬件规格建议（按目标带宽倒推）

| 部件 | 建议 | 理由 |
|---|---|---|
| **CPU** | ★高对称带宽场景的**第一要素**。要能把 QUIC 加解密喂到目标带宽（2.5Gbps 需较强多核 + AES-NI）。**别用树莓派/廉价 CPU 受限机** | QUIC 用户态加解密吃 CPU，官方明确警告 |
| **内存** | 16GB 对家宽出口**绰绰有余**（见 §7 评估）；大并发云出口可上 32GB | 每连接接收窗口峰值 ~28MB，但多数空闲 |
| **网卡** | 2.5G/多 Gig 网卡 + **多队列**（喂满 2.5G 对称必须）；WAN/LAN 分口 | 单队列在高 PPS 下单核打满 |
| **供电** | ★**UPS**（尤其 GR240BG 桥接掉回坑 + 形态 B 软路由挂=断网）| 减少重启 = 减少桥接失效 / 断网 |

---

## 6. 内核调优清单（形态 B / 高带宽必做）

榨满 2.5G 对称、扛大量 QUIC 并发，Linux 默认值不够，需调：

```
# UDP buffer（对齐 hysteria2 建议的 16MB 系统缓冲）
net.core.rmem_max / net.core.wmem_max        → 提到 16MB+
net.core.rmem_default / wmem_default

# conntrack 会话表（大量 UDP 打洞会话）
net.netfilter.nf_conntrack_max               → 提到百万级
net.netfilter.nf_conntrack_buckets

# 软中断 / 多队列分散（喂满 2.5G 单核会打满）
RPS / RFS 开启；网卡多队列绑核（ethtool -L / irq affinity）

# 文件描述符（大量连接）
ulimit -n / LimitNOFILE in systemd unit
```

> 具体数值待真机压测后回填本节。**这些值要随目标带宽和实测瓶颈调，不是抄一遍就最优。**

---

## 7. 容量实测方法（建完必做，得出真实可服务用户数）

容量是**动态的、按这台机器实测得出的**，不能拍脑袋。实测流程：

```
1. speedtest 实测上行带宽（第一天花板）
2. 压测：模拟 N 个并发 hy2 连接持续传输，逐步加压
3. 监测哪个先到顶：
   - 上行带宽利用率（低对称带宽场景先到）
   - CPU（高对称带宽场景先到 ← Optimum 2.5G 属此类）
   - 内存（conn × 接收窗口）
   - conntrack 表占用（ss / conntrack -C）
4. 先到顶的那一项 × 安全余量 = 该出口的真实可服务用户数
5. 把这个容量喂回 obs 平面，作为负载感知 LB 的调度依据
```

> **关键指标选择**：realm 出口调度应优先看 **上行带宽利用率 / CPU 利用率**，而非 `active_connections`（连接数）——因为带宽或 CPU 先于连接数到顶（[[FLEET_MANAGER_DESIGN]] §5.5）。

---

## 8. 16GB 服务器并发简评（内存维度）

| 估法 | 假设 | 16GB 可承载 |
|---|---|---|
| 保守（每连接吃满 28MB 窗口） | 全部满速 | (16-4)GB ÷ 28MB ≈ **~430 连接** |
| 实际（混合负载，均摊 ~3MB） | 多数空闲/低速 | 12GB ÷ 3MB ≈ **~4000 连接** |

**判断：内存几乎从不是 realm 出口的真瓶颈。**
- 低对称带宽家宽：带宽先于内存几十倍到顶。
- **Optimum 2.5G 对称：2.5Gbps 在内存到顶前先吃满，且 CPU 通常先于内存到瓶颈**——所以 16GB 对你的场景**够用**，真正要堆的是 **CPU**。

---

## 9. 待办 / 待实测（建出口时回填）

- [ ] **确认 Optimum 2.5G 实际网关型号**（是否 GR240BG → 桥接重启掉回风险）。
- [ ] 实测 2.5G 套餐**真实对称上行**（speedtest upload）。
- [ ] 选定路由形态（自有高性能机 → 形态 B 一体化软路由）。
- [ ] §6 内核调优真机回填具体数值。
- [ ] §7 压测得出该出口真实可服务用户数。
- [ ] 形态 B 的"软路由挂=断网"故障域：配 UPS + 桥接状态监控。

---

## 相关来源

- [Hysteria 2 Performance（官方，硬件瓶颈）](https://v2.hysteria.network/docs/advanced/Performance/)
- [sing-box Hysteria2 inbound（无连接数硬上限）](https://sing-box.sagernet.org/configuration/inbound/hysteria2/)
- [Using your own router with Optimum/Altice — bridge mode 指南](https://asheroto.medium.com/using-your-own-router-firewall-with-optimum-suddenlink-and-how-to-disable-wi-fi-too-e705862497de)
- [FiberGateway GR240BG bridge 重启掉回讨论（DSLReports）](https://www.dslreports.com/forum/r32573603-FiberGateway-GR240BG)
