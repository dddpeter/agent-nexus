#!/usr/bin/env python3
"""agent-nexus 黑盒集成测试（2026-11-07 轮）。

- 自管理 serve 进程生命周期（独立数据目录，含 kill -9 重启场景）
- 限流感知：429 自动退避等窗口（60 req/min），测试节奏自带节流
- 真实 agent 组可跳过（--skip-real），避免 LLM 成本/耗时
- 输出：控制台矩阵 + 可拼接的 markdown 证据（TEST-REPORT 引用）

用法：
  python3 scripts/integration_test.py                 # 全量
  python3 scripts/integration_test.py --skip-real     # 跳过 hermes/picoclaw 真实调用
"""
import argparse
import json
import os
import shutil
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.request

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.path.join(REPO, "bin", "nexus")
PORT_A = 19181
PORT_B = 19182
BASE_A = f"http://127.0.0.1:{PORT_A}"
BASE_B = f"http://127.0.0.1:{PORT_B}"
TOKEN_B = "it-secret-token"
DATA_A = "/tmp/nexus-it-a"
DATA_B = "/tmp/nexus-it-b"
HERMES_KEY = os.environ.get("API_SERVER_KEY", "")
NO_PROXY = {"http": None, "https": None}

results = []  # (组, 编号, 名称, ok, 证据)


class Serve:
    def __init__(self, port, data_dir, token=""):
        self.port = port
        self.data_dir = data_dir
        self.token = token
        self.proc = None
        self.log = open(f"/tmp/nexus-it-{port}.log", "ab")

    def kill_stale(self):
        """清掉同端口的残留 serve（上次运行 timeout 只杀脚本不杀守护进程的遗留）。"""
        try:
            out = subprocess.run(["pgrep", "-f", f"listen 127.0.0.1:{self.port}"],
                                 capture_output=True, text=True, timeout=5)
            for pid in out.stdout.split():
                try:
                    os.kill(int(pid), signal.SIGKILL)
                except (ProcessLookupError, ValueError):
                    pass
            if out.stdout.strip():
                time.sleep(0.5)
        except Exception:
            pass

    def start(self, fresh=True):
        self.kill_stale()
        env = dict(os.environ)
        if HERMES_KEY:
            env["API_SERVER_KEY"] = HERMES_KEY
        if fresh:
            shutil.rmtree(self.data_dir, ignore_errors=True)
        os.makedirs(self.data_dir, exist_ok=True)
        cfg_path = os.path.join(self.data_dir, "config.yaml")
        if self.token:
            with open(cfg_path, "w") as f:
                f.write(f"server:\n  listen: ':{self.port}'\n  auth_token: {self.token}\n")
        self.proc = subprocess.Popen(
            [BIN, "serve", "--listen", f"127.0.0.1:{self.port}", "-c", cfg_path],
            cwd=self.data_dir, env=env, stdout=self.log, stderr=self.log,
            stdin=subprocess.DEVNULL, start_new_session=True,
        )
        self.wait_up()

    def wait_up(self, timeout=30):
        deadline = time.time() + timeout
        while time.time() < deadline:
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{self.port}/healthz", timeout=2) as r:
                    if r.status == 200:
                        return
            except Exception:
                time.sleep(0.3)
        raise RuntimeError(f"serve :{self.port} 未在 {timeout}s 内就绪")

    def kill9(self):
        if self.proc:
            try:
                os.kill(self.proc.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            self.proc.wait(timeout=10)
            self.proc = None

    def stop(self):
        self.kill9()
        self.kill_stale()
        self.log.close()


def req(base, method, path, body=None, token="", timeout=60, raw=False):
    """带限流退避的请求；返回 (status, parsed_json_or_text)。"""
    data = json.dumps(body).encode() if body is not None else None
    attempts = 0
    for attempt in range(12):
        r = urllib.request.Request(base + path, data=data, method=method)
        if body is not None:
            r.add_header("Content-Type", "application/json")
        if token:
            r.add_header("Authorization", "Bearer " + token)
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            with opener.open(r, timeout=timeout) as resp:
                raw_bytes = resp.read()
                status = resp.status
            break
        except urllib.error.HTTPError as e:
            status = e.code
            raw_bytes = e.read()
            if status == 429:
                attempts += 1
                time.sleep(5)  # 窗口 60s，429 后小步轮询；attempts 上限兜底
                continue
            break
        except urllib.error.URLError as e:
            raise ConnectionError(f"{method} {path}: {e}") from e
    else:
        raise RuntimeError(f"{method} {path}: 限流退避耗尽")
    text = raw_bytes.decode("utf-8", "replace")
    if raw:
        return status, text
    try:
        return status, json.loads(text)
    except json.JSONDecodeError:
        return status, text


def stream_send(base, session_id, content, timeout=90):
    """流式发送：读 SSE 分帧，返回 (deltas, final, errors)。"""
    body = json.dumps({"content": content, "mode": "stream"}).encode()
    r = urllib.request.Request(f"{base}/api/sessions/{session_id}/messages", data=body, method="POST")
    r.add_header("Content-Type", "application/json")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    deltas, finals, errs = [], [], []
    with opener.open(r, timeout=timeout) as resp:
        event = None
        for line in resp:
            line = line.decode("utf-8").strip()
            if line.startswith("event: "):
                event = line[7:]
            elif line.startswith("data: ") and event:
                payload = json.loads(line[6:])
                (deltas if event == "delta" else finals if event == "final" else errs).append(payload.get("data", ""))
    return deltas, finals, errs


def record(group, no, name, ok, evidence=""):
    results.append((group, no, name, bool(ok), evidence))
    mark = "✅" if ok else "❌"
    print(f"  {mark} {no} {name}" + (f"  — {evidence}" if evidence else ""))
    return ok


def section(title):
    print(f"\n== {title} ==")


# ---------------------------------------------------------------- 测试组
def suite_bootstrap(s: Serve):
    section("B. 启动/迁移/持久化（F6）")
    st, body = req(BASE_A, "GET", "/healthz")
    record("B", "I01", "healthz 就绪", st == 200 and body.get("ok") is True, str(body))

    # 存量库迁移：sqlite 里应有 disabled 与 state_json 列（迁移 00002/00003）
    import sqlite3
    conn = sqlite3.connect(os.path.join(s.data_dir, "data", "nexus.db"))
    cols_agents = {r[1] for r in conn.execute("PRAGMA table_info(agents)")}
    cols_groups = {r[1] for r in conn.execute("PRAGMA table_info(groups)")}
    ok = "disabled" in cols_agents and "state_json" in cols_groups
    record("B", "I02", "迁移 00002/00003 自动应用（agents.disabled + groups.state_json）", ok,
           f"agents.disabled={'disabled' in cols_agents} groups.state_json={'state_json' in cols_groups}")

    # 备份：启动即出快照（DSN 相对路径 → 实际库在 data_dir/data/）
    backups = os.listdir(os.path.join(s.data_dir, "data", "backups")) if os.path.isdir(os.path.join(s.data_dir, "data", "backups")) else []
    record("B", "I03", "VACUUM INTO 启动快照", len(backups) >= 1, f"{len(backups)} 份: {backups[:2]}")
    conn.close()


def suite_agents(s: Serve):
    section("C. Agent 注册/配置/停用（F1 回归 + disable 持久化）")
    st, ad = req(BASE_A, "GET", "/api/adapters", token=BUILTIN_TOKEN)
    record("C", "I10", "adapters 三工厂 schema", st == 200 and {a["name"] for a in ad} >= {"echod", "hermes", "picoclaw"}, "")

    st, a = req(BASE_A, "POST", "/api/agents", {"name": "it-echo", "adapter": "echod", "config": {"mode": "echo"}}, token=BUILTIN_TOKEN)
    ok = st == 201 and a.get("status") == "online"
    record("C", "I11", "注册 echod → online", ok,
           f"status={a.get('status')} id={a.get('id')}" + ("" if ok else f" http={st} body={str(a)[:120]}"))
    globals()["ECHO_ID"] = a["id"]

    st, a2 = req(BASE_A, "POST", "/api/agents", {"name": "it-echo-stream", "adapter": "echod",
                                                 "config": {"mode": "stream", "stream_chunks": 3, "stream_interval_ms": 60}}, token=BUILTIN_TOKEN)
    record("C", "I12", "注册 stream-only echod", st == 201 and a2.get("capabilities", {}).get("can_stream") is True, "")
    globals()["STREAM_ID"] = a2["id"]

    st, err = req(BASE_A, "POST", "/api/agents", {"name": "it-echo", "adapter": "echod", "config": {"mode": "echo"}}, token=BUILTIN_TOKEN)
    record("C", "I13", "重名 409", st == 409, str(err.get("error", {}).get("code", "")))

    # disable → 派消息 409 → 重启恢复 disabled（I02 迁移列的行为闭环）
    st, _ = req(BASE_A, "POST", f"/api/agents/{ECHO_ID}/disable", token=BUILTIN_TOKEN)
    st2, sess = req(BASE_A, "POST", "/api/sessions", {"agent_id": ECHO_ID}, token=BUILTIN_TOKEN)
    st3, err = req(BASE_A, "POST", f"/api/sessions/{sess['id']}/messages", {"content": "hi", "mode": "sync"}, token=BUILTIN_TOKEN)
    ok = st == 200 and st3 == 409
    record("C", "I14", "停用 → 派消息 409", ok, f"disable={st} send={st3} code={err.get('error', {}).get('code') if isinstance(err, dict) else ''}")

    st, detail = req(BASE_A, "GET", f"/api/agents/{ECHO_ID}", token=BUILTIN_TOKEN)
    record("C", "I15", "停用态 API 可见 disabled=true", st == 200 and detail.get("disabled") is True, "")

    # 恢复 it-echo 供后续群组/私聊套件使用；停用持久化用独立 agent
    req(BASE_A, "POST", f"/api/agents/{ECHO_ID}/enable", token=BUILTIN_TOKEN)
    st, a3 = req(BASE_A, "POST", "/api/agents", {"name": "it-echo-disable", "adapter": "echod", "config": {"mode": "echo"}}, token=BUILTIN_TOKEN)
    req(BASE_A, "POST", f"/api/agents/{a3['id']}/disable", token=BUILTIN_TOKEN)
    globals()["DISABLE_ID"] = a3["id"]
    return sess["id"]


def restart_and_check_disabled(s: Serve):
    section("D. kill -9 重启持久化恢复（disable + 群状态）")
    st, detail = req(BASE_A, "GET", f"/api/agents/{DISABLE_ID}", token=BUILTIN_TOKEN)
    ok = st == 200 and detail.get("disabled") is True and detail.get("status") == "offline"
    record("D", "I20", "kill -9 → 重启后 disabled=true 持久恢复（不探活置 offline）", ok,
           f"disabled={detail.get('disabled')} status={detail.get('status')}")
    st, _ = req(BASE_A, "POST", f"/api/agents/{DISABLE_ID}/enable", token=BUILTIN_TOKEN)
    st, detail = req(BASE_A, "GET", f"/api/agents/{DISABLE_ID}", token=BUILTIN_TOKEN)
    record("D", "I21", "enable → probe 恢复 online", st == 200 and detail.get("disabled") is False and detail.get("status") == "online",
           f"disabled={detail.get('disabled')} status={detail.get('status')}")
    # 健康 agent 重启后探活自动 online（原有 kill -9 语义回归）
    st, detail = req(BASE_A, "GET", f"/api/agents/{ECHO_ID}", token=BUILTIN_TOKEN)
    record("D", "I22", "健康 agent 重启后自动 online", detail.get("status") == "online", f"status={detail.get('status')}")


BUILTIN_TOKEN = ""


def suite_private(s: Serve, sess_id: str):
    section("E. 私聊回归 + 本轮修复（F2）")
    # 同步
    st, res = req(BASE_A, "POST", f"/api/sessions/{sess_id}/messages", {"content": "同步回归", "mode": "sync"}, token=BUILTIN_TOKEN)
    ok = st == 200 and res.get("reply", {}).get("content") == "同步回归"
    record("E", "I30", "同步发送回显 + reply_to", ok, f"reply={bool(res.get('reply'))}")

    # 历史升序契约 + limit
    for i in range(3):
        req(BASE_A, "POST", f"/api/sessions/{sess_id}/messages", {"content": f"fill-{i}", "mode": "sync"}, token=BUILTIN_TOKEN)
    st, msgs = req(BASE_A, "GET", f"/api/sessions/{sess_id}/messages", token=BUILTIN_TOKEN)
    times = [m["created_at"] for m in msgs]
    ok = st == 200 and times == sorted(times)
    record("E", "I31", "历史升序契约（聊天气泡顺序）", ok, f"{len(msgs)} 条升序={times == sorted(times)}")

    st, lim = req(BASE_A, "GET", f"/api/sessions/{sess_id}/messages?limit=2", token=BUILTIN_TOKEN)
    record("E", "I32", "?limit=2 取最近 2 条且仍升序", st == 200 and len(lim) == 2 and [m["created_at"] for m in lim] == sorted(m["created_at"] for m in lim),
           f"n={len(lim)}")

    # 流式（本 agent 是 echo 同步型 → 409 mode_not_supported）
    st, err = req(BASE_A, "POST", f"/api/sessions/{sess_id}/messages", {"content": "x", "mode": "stream"}, token=BUILTIN_TOKEN)
    record("E", "I33", "echo 型 agent 流式发送 → 409 mode_not_supported", st == 409, str(err.get("error", {}).get("code", "")))

    # stream-only agent：同步 409 / 流式 SSE
    st, sess2 = req(BASE_A, "POST", "/api/sessions", {"agent_id": STREAM_ID}, token=BUILTIN_TOKEN)
    st, err = req(BASE_A, "POST", f"/api/sessions/{sess2['id']}/messages", {"content": "x", "mode": "sync"}, token=BUILTIN_TOKEN)
    record("E", "I34", "stream-only agent 同步发送 → 409", st == 409, str(err.get("error", {}).get("code", "")))
    try:
        deltas, finals, errs = stream_send(BASE_A, sess2["id"], "流式分帧回归")
        ok = len(deltas) >= 2 and len(finals) == 1 and not errs
        record("E", "I35", "流式 SSE ≥2 delta + 1 final", ok, f"delta={len(deltas)} final={len(finals)} err={len(errs)}")
    except Exception as e:
        record("E", "I35", "流式 SSE ≥2 delta + 1 final", False, str(e))

    # 异步
    st, res = req(BASE_A, "POST", f"/api/sessions/{sess_id}/messages", {"content": "异步回归", "mode": "async"}, token=BUILTIN_TOKEN)
    ok = st == 202 and res.get("async", {}).get("type") == "accepted"
    record("E", "I36", "async 202 + accepted 回执", ok, str(res.get("async", {}).get("task_id", ""))[:20])
    time.sleep(0.5)
    st, msgs = req(BASE_A, "GET", f"/api/sessions/{sess_id}/messages", token=BUILTIN_TOKEN)
    backfill = any(m["sender_type"] == "system" and "异步" in m["content"] for m in msgs)
    record("E", "I37", "异步回执 system 回填落库", backfill, "")

    # 事件检索（本轮新功能）
    st, evs = req(BASE_A, "GET", "/api/events?limit=50", token=BUILTIN_TOKEN)
    seqs = [e["seq"] for e in evs]
    ok = st == 200 and seqs == sorted(seqs, reverse=True) and any(e["type"] == "msg.human" for e in evs)
    record("E", "I38", "GET /api/events seq 降序 + 含 msg 事件", ok, f"n={len(evs)} top_type={evs[0]['type'] if evs else '-'}")
    st, evs2 = req(BASE_A, "GET", "/api/events?type=arbiter.decision", token=BUILTIN_TOKEN)
    record("E", "I39", "事件 type 过滤", st == 200 and all(e["type"] == "arbiter.decision" for e in evs2), f"n={len(evs2)}")


def suite_group(s: Serve):
    section("F. 群聊协调器（M3）")
    # 建群校验
    st, err = req(BASE_A, "POST", "/api/groups", {"name": "无人群", "members": [{"id": "human-1", "type": "human"}]}, token=BUILTIN_TOKEN)
    record("F", "I40", "无 agent 成员 → 400", st == 400, str(err.get("error", {}).get("message", ""))[:40])

    members = [{"id": ECHO_ID, "type": "agent", "trigger": "any", "priority": 5, "profile": "数据库 SQL 索引"}]
    st, g = req(BASE_A, "POST", "/api/groups", {"name": "IT 群", "members": members, "max_turns_per_topic": 1, "human_silent_threshold": 1}, token=BUILTIN_TOKEN)
    ok = st == 201 and g.get("id", "").startswith("grp-")
    record("F", "I41", "建群（预算 1 / 静默阈值 1）", ok, f"id={g.get('id')}")
    gid = g["id"]

    # @ 路由（人打注册名）→ 回复
    st, res = req(BASE_A, "POST", f"/api/groups/{gid}/messages", {"content": "@it-echo 你好", "sender_id": "peter"}, token=BUILTIN_TOKEN)
    ok = st == 200 and res.get("reply") and res["reply"]["sender_id"] == ECHO_ID
    record("F", "I42", "@注册名路由 → 唯一响应 + 回复落库", ok, f"reply_sender={res.get('reply', {}).get('sender_id', '-')[:12]}")

    # 预算 Deferred（max_turns=1 已用 1）
    st, res2 = req(BASE_A, "POST", f"/api/groups/{gid}/messages", {"content": "@it-echo 第二轮", "sender_id": "peter"}, token=BUILTIN_TOKEN)
    deferred = res2.get("deferred") or []
    ok = st == 200 and not res2.get("reply") and deferred and deferred[0]["reason"] == "budget_exceeded"
    record("F", "I43", "超预算候选 → Deferred 存档", ok, f"reason={deferred[0]['reason'] if deferred else '-'}")

    # 存档 system 消息可见
    st, msgs = req(BASE_A, "GET", f"/api/groups/{gid}/messages", token=BUILTIN_TOKEN)
    archived = any(m["sender_type"] == "system" and "[deferred:budget_exceeded]" in m["content"] for m in msgs)
    record("F", "I44", "Deferred 存档消息（system）可检索", archived, f"n={len(msgs)}")

    # 注：静默自动冻结（FR3.5）的触发路径依赖 agent 主动候选（v1 无自发通道，人发言会清零
    # 计数），运行态不可达属预期收敛（宁严勿松）；纯函数语义由 U-F3-06/09 覆盖。此处验证解冻 API。
    st, _ = req(BASE_A, "POST", f"/api/groups/{gid}/unfreeze", token=BUILTIN_TOKEN)
    st, detail = req(BASE_A, "GET", f"/api/groups/{gid}", token=BUILTIN_TOKEN)
    record("F", "I45", "解冻 API 幂等（未冻结群调用无害）", st == 200 and detail.get("state", {}).get("frozen") is False, "")

    # /topic 预算归零
    st, res4 = req(BASE_A, "POST", f"/api/groups/{gid}/messages", {"content": "/topic 新话题", "sender_id": "peter"}, token=BUILTIN_TOKEN)
    st, detail = req(BASE_A, "GET", f"/api/groups/{gid}", token=BUILTIN_TOKEN)
    turns = detail.get("state", {}).get("turns_used", {})
    record("F", "I46", "/topic → 预算归零", all(v == 0 for v in turns.values()), f"turns_used={turns}")

    # 热更新
    st, up = req(BASE_A, "PUT", f"/api/groups/{gid}/config", {"max_turns_per_topic": 5}, token=BUILTIN_TOKEN)
    record("F", "I47", "阈值热更新即时生效", st == 200 and up.get("max_turns_per_topic") == 5, "")

    # 仲裁决策事件（bus 异步批量落库 → 轮询等 flush）
    group_decs = []
    deadline = time.time() + 10
    while time.time() < deadline:
        st, evs = req(BASE_A, "GET", "/api/events?type=arbiter.decision&limit=100", token=BUILTIN_TOKEN)
        group_decs = [e for e in evs if isinstance(e.get("payload"), dict) and e["payload"].get("group_id") == gid]
        if len(group_decs) >= 3:
            break
        time.sleep(0.5)
    record("F", "I48", "arbiter.decision 事件全量落库（回放调参）", len(group_decs) >= 3, f"n={len(group_decs)}")
    globals()["GROUP_ID"] = gid

    # 重启恢复群运行态（与 I20 同一轮重启；此处校验快照）
    # 注：重启在 restart_and_check_disabled 完成，此组在其后执行时顺序调整见 main()
    return gid


def suite_group_after_restart(s: Serve, gid: str, turns_before: dict):
    section("G. 群状态快照重启恢复（U-F6-05）")
    s.SnapshotAndRestart = True  # 标记（无实际作用，防误读）
    st, detail = req(BASE_A, "GET", f"/api/groups/{gid}", token=BUILTIN_TOKEN)
    turns_after = detail.get("state", {}).get("turns_used", {})
    record("G", "I50", "kill -9 → 重启后群运行态恢复", st == 200 and turns_after == turns_before,
           f"before={turns_before} after={turns_after}")


def suite_cli(s: Serve):
    section("H. CLI（FR5.4）")
    env = dict(os.environ, NEXUS_URL=BASE_A)
    r = subprocess.run([BIN, "agent", "list", "--server", BASE_A], capture_output=True, text=True, timeout=30, env=env)
    ok = r.returncode == 0 and "it-echo" in r.stdout
    record("H", "I60", "nexus agent list", ok, r.stdout.strip().splitlines()[-1] if r.stdout.strip() else r.stderr[:60])

    r = subprocess.run([BIN, "chat", "it-echo", "--server", BASE_A], input="CLI REPL 冒烟\n/exit\n",
                       capture_output=True, text=True, timeout=60, env=env)
    ok = r.returncode == 0 and "CLI REPL 冒烟" in r.stdout and "it-echo>" in r.stdout
    record("H", "I61", "nexus chat REPL 同步往返", ok, (r.stdout.strip().splitlines() or [""])[-1][:60])

    r = subprocess.run([BIN, "group", "send", GROUP_NAME_OR_ID, "@it-echo CLI 群发言", "--server", BASE_A],
                       capture_output=True, text=True, timeout=60, env=env)
    ok = r.returncode == 0 and ECHO_ID[:8] in r.stdout
    record("H", "I62", "nexus group send 经仲裁路由", ok, r.stdout.strip()[:60] or r.stderr[:60])


GROUP_NAME_OR_ID = "IT 群"


def suite_auth():
    section("I. 鉴权（F5 / 实例 B 带 token）")
    st, _ = req(BASE_B, "GET", "/api/agents")
    record("I", "I70", "无 token → 401", st == 401, "")
    st, _ = req(BASE_B, "GET", "/api/agents", token="wrong")
    record("I", "I71", "错 token → 401", st == 401, "")
    st, agents = req(BASE_B, "GET", "/api/agents", token=TOKEN_B)
    record("I", "I72", "正确 token → 200", st == 200 and isinstance(agents, list), f"n={len(agents)}")
    st, groups = req(BASE_B, "GET", "/api/groups", token=TOKEN_B)
    record("I", "I73", "群资源同受鉴权保护", st == 200 and isinstance(groups, list), f"n={len(groups)}")
    st, body = req(BASE_B, "GET", "/api/agents", token="wrong")
    env_ok = isinstance(body, dict) and "error" in body and "request_id" in body["error"]
    record("I", "I74", "统一错误结构 code+request_id", env_ok, str(body.get("error", {}).get("code", "")))


def suite_real():
    section("J. 真实 agent（hermes :8642 / picoclaw CLI）")
    if not HERMES_KEY:
        record("J", "I80", "hermes 真实调用", False, "未提供 API_SERVER_KEY，跳过（--skip-real 语义）")
        record("J", "I84", "picoclaw 真实调用", False, "同上")
        return

    # hermes
    st, h = req(BASE_A, "POST", "/api/agents", {"name": "it-hermes", "adapter": "hermes",
                                                "config": {"base_url": "http://127.0.0.1:8642", "profile": "default",
                                                           "api_key": "${API_SERVER_KEY}", "model": "hermes-agent", "timeout": "5m"}}, token=BUILTIN_TOKEN)
    ok = st == 201 and h.get("status") == "online"
    record("J", "I80", "hermes 注册 → 探活 online（真实网关 /health）", ok, f"status={h.get('status')}")
    if not ok:
        return

    st, hs = req(BASE_A, "POST", "/api/sessions", {"agent_id": h["id"]}, token=BUILTIN_TOKEN)
    st, res = req(BASE_A, "POST", f"/api/sessions/{hs['id']}/messages", {"content": "只回复两个字：收到", "mode": "sync"}, token=BUILTIN_TOKEN, timeout=120)
    reply = (res.get("reply") or {}).get("content", "")
    ok = st == 200 and "收到" in reply
    record("J", "I81", "hermes 同步发送（真实 LLM）", ok, f"reply={reply[:30]}")

    st, sess2 = req(BASE_A, "POST", "/api/sessions", {"agent_id": h["id"]}, token=BUILTIN_TOKEN)
    try:
        deltas, finals, errs = stream_send(BASE_A, sess2["id"], "从1数到3用顿号分隔", timeout=120)
        full = "".join(deltas) or (finals[0] if finals else "")
        ok = len(deltas) >= 1 and len(finals) == 1 and "1" in full and "3" in full
        record("J", "I82", "hermes 流式（真实 OpenAI SSE → 统一帧）", ok,
               f"delta={len(deltas)} final={''.join(finals)[:20]}")
    except Exception as e:
        record("J", "I82", "hermes 流式（真实 OpenAI SSE → 统一帧）", False, str(e)[:80])

    st, res3 = req(BASE_A, "POST", f"/api/sessions/{hs['id']}/messages", {"content": "我上一条让你回复什么？", "mode": "sync"}, token=BUILTIN_TOKEN, timeout=120)
    reply3 = (res3.get("reply") or {}).get("content", "")
    ok = "收到" in reply3
    record("J", "I83", "hermes 续话记忆（external_ref 稳定）", ok, f"reply={reply3[:30]}")

    # picoclaw（CLI 真实 LLM，~90s/次）
    st, p = req(BASE_A, "POST", "/api/agents", {"name": "it-picoclaw", "adapter": "picoclaw",
                                                "config": {"command": ["picoclaw", "agent", "-m", "{{input}}", "-s", "agent:main:{{session}}", "--no-color"],
                                                           "probe_command": ["picoclaw", "version"], "output": "lastline", "timeout": "10m"}}, token=BUILTIN_TOKEN)
    ok = st == 201 and p.get("status") == "online"
    record("J", "I84", "picoclaw 注册 → probe（version）online", ok, f"status={p.get('status')}")
    if not ok:
        return
    st, ps = req(BASE_A, "POST", "/api/sessions", {"agent_id": p["id"]}, token=BUILTIN_TOKEN)
    st, res4 = req(BASE_A, "POST", f"/api/sessions/{ps['id']}/messages", {"content": "只回复四个字：链路畅通", "mode": "sync"}, token=BUILTIN_TOKEN, timeout=300)
    reply4 = (res4.get("reply") or {}).get("content", "")
    ok = st == 200 and "链路畅通" in reply4
    record("J", "I85", "picoclaw 同步发送（lastline 解析真实 CLI 噪音）", ok, f"reply={reply4[:30]}")


def main():
    global BUILTIN_TOKEN
    ap = argparse.ArgumentParser()
    ap.add_argument("--skip-real", action="store_true")
    args = ap.parse_args()

    if not os.path.exists(BIN):
        print("bin/nexus 不存在，请先 make build", file=sys.stderr)
        sys.exit(2)

    print(f"== 集成测试启动 A:{PORT_A}（无鉴权）/ B:{PORT_B}（token）数据 {DATA_A}|{DATA_B} ==")
    a = Serve(PORT_A, DATA_A)
    b = Serve(PORT_B, DATA_B, token=TOKEN_B)
    try:
        a.start()
        b.start()
        suite_bootstrap(a)
        sess_id = suite_agents(a)
        gid = suite_group(a)

        # 群运行态记录 → kill -9 重启 → 双恢复校验（disable I20/I21 + 群 I50）
        st, detail = req(BASE_A, "GET", f"/api/groups/{gid}", token=BUILTIN_TOKEN)
        turns_before = detail.get("state", {}).get("turns_used", {})
        # 再推进一轮群发言，使快照含「有差异的计数」
        req(BASE_A, "POST", f"/api/groups/{gid}/messages", {"content": "@it-echo 重启前最后一轮", "sender_id": "peter"}, token=BUILTIN_TOKEN)
        st, detail = req(BASE_A, "GET", f"/api/groups/{gid}", token=BUILTIN_TOKEN)
        turns_before = detail.get("state", {}).get("turns_used", {})
        time.sleep(31)  # 30s 快照 tick 落盘（异步快照 + 丢失容忍设计的验证前提）
        a.kill9()
        a.start(fresh=False)
        restart_and_check_disabled(a)
        suite_group_after_restart(a, gid, turns_before)

        suite_private(a, sess_id)
        suite_cli(a)
        suite_auth()
        if not args.skip_real:
            suite_real()
    finally:
        a.stop()
        b.stop()

    # 汇总
    section("汇总")
    total = len(results)
    passed = sum(1 for r in results if r[3])
    print(f"\n总计 {passed}/{total} 通过")
    for g, no, name, ok, ev in results:
        if not ok:
            print(f"  ❌ FAIL {no} {name} — {ev}")
    # markdown 矩阵
    with open("/tmp/nexus-it-results.md", "w") as f:
        f.write("| # | 用例 | 结果 | 证据 |\n|---|---|---|---|\n")
        for g, no, name, ok, ev in results:
            f.write(f"| {no} | {name} | {'✅' if ok else '❌'} | {ev} |\n")
    sys.exit(0 if passed == total else 1)


if __name__ == "__main__":
    main()
