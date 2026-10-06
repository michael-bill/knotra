#!/usr/bin/env python3
"""Compare production backends using disposable schemas and local HTTP fixtures."""

import argparse
import base64
import contextlib
import hashlib
import http.client
import http.server
import json
import os
import pathlib
import platform
import signal
import socket
import subprocess
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


def command(*args):
    return subprocess.check_output(args, text=True).strip()


def sql(args, query):
    return command(
        "docker",
        "exec",
        "-e",
        "PGOPTIONS=-c client_min_messages=warning",
        args.postgres_container,
        "psql",
        "-XAt",
        "-v",
        "ON_ERROR_STOP=1",
        "-U",
        args.db_user,
        "-d",
        args.db_name,
        "-c",
        query,
    )


def docker_stats(path, name):
    with contextlib.closing(
        http.client.HTTPConnection("docker", timeout=5)
    ) as connection:
        sock = socket.socket(socket.AF_UNIX)
        sock.settimeout(5)
        sock.connect(path)
        connection.sock = sock
        connection.request(
            "GET",
            f"/containers/{urllib.parse.quote(name)}/stats?stream=false&one-shot=true",
        )
        response = connection.getresponse()
        if response.status != 200:
            raise RuntimeError(f"Docker stats {name}: HTTP {response.status}")
        data = json.load(response)
        memory = data["memory_stats"]
        cache = memory["stats"].get(
            "inactive_file", memory["stats"].get("total_inactive_file", 0)
        )
        return {
            "working_set_bytes": max(0, memory["usage"] - cache),
            "cpu_seconds": data["cpu_stats"]["cpu_usage"]["total_usage"] / 1e9,
        }


def process_stats(pid):
    rss, cpu = command("ps", "-o", "rss=,cputime=", "-p", str(pid)).split()
    days = 0
    if "-" in cpu:
        days, cpu = cpu.split("-")
    seconds = float(days) * 86400
    for index, field in enumerate(reversed(cpu.split(":"))):
        seconds += float(field) * 60**index
    return {"rss_bytes": int(rss) * 1024, "cpu_seconds": seconds}


def database_stats(args, schema):
    query = f"""SELECT json_build_object(
        'wal_lsn',pg_current_wal_insert_lsn()::text,
        'query_calls',(SELECT COALESCE(sum(calls),0) FROM pg_stat_statements WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())),
        'query_exec_ms',(SELECT COALESCE(sum(total_exec_time),0) FROM pg_stat_statements WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())),
        'transactions',xact_commit+xact_rollback,
        'connections',(SELECT count(*) FROM pg_stat_activity WHERE application_name='{schema}'),
        'schema_bytes',(SELECT COALESCE(sum(pg_total_relation_size(c.oid)),0) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='{schema}' AND c.relkind IN ('r','m'))
        ) FROM pg_stat_database WHERE datname=current_database()"""
    result = json.loads(sql(args, query))
    high, low = result.pop("wal_lsn").split("/")
    result["wal_bytes"] = int(high, 16) * 2**32 + int(low, 16)
    return result


def allocated_storage(container, path):
    return (
        int(command("docker", "exec", container, "du", "-sk", path).split()[0]) * 1024
    )


def statement_stats(args):
    rows = json.loads(
        sql(
            args,
            """
        SELECT COALESCE(json_agg(stats),'[]') FROM (
            SELECT queryid::text AS id,calls,total_exec_time AS exec_ms,
                wal_bytes,wal_records,wal_fpi,left(query,2000) AS query
            FROM pg_stat_statements
            WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
        ) stats
    """,
        )
    )
    return {row["id"]: row for row in rows}


def request(base, route, body=None, timeout=120):
    data = None if body is None else json.dumps(body, separators=(",", ":")).encode()
    headers = {"Content-Type": "application/json"}
    if body is not None:
        headers["Idempotency-Key"] = uuid.uuid4().hex
    try:
        with urllib.request.urlopen(
            urllib.request.Request(base + "/v1" + route, data=data, headers=headers),
            timeout=timeout,
        ) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        raise RuntimeError(
            f"{route}: HTTP {error.code}: {error.read(4096).decode(errors='replace')}"
        ) from error


def free_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def wait_run(base, run_id, status, timeout=600):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        run = request(base, "/runs/" + run_id)["run"]
        if run["status"] == status:
            return run
        if run["status"] in ("succeeded", "failed", "cancelled"):
            raise RuntimeError(
                f"run {run_id}: expected {status}, got {run['status']}: {run.get('diagnostics')}"
            )
        time.sleep(0.25)
    raise TimeoutError(f"run {run_id}: {status}")


def workload(name):
    schema = {"type": "integer"}
    models = {"writer": {"connection": "cloud", "requires": ["structuredOutput"]}}
    nodes = {}
    if name == "short_dag":
        for index in range(64):
            node = {
                "type": "llm",
                "llm": {"model": "writer", "prompt": {"text": "fixture answer"}},
                "outputs": {"answer": {"schema": schema}},
            }
            if index >= 16:
                node["needs"] = [f"n{index - 16:03}"]
            nodes[f"n{index:03}"] = node
        target, expected, calls = "nodes.n063.outputs.answer", 42, 64
    elif name == "large_controls":
        for index in range(1000):
            nodes[f"switch_{index:04}"] = {
                "type": "switch",
                "switch": {
                    "cases": [{"name": "never", "when": "false"}],
                    "default": "done",
                },
            }
        for index in range(4):
            nodes[f"model_{index}"] = {
                "type": "llm",
                "llm": {
                    "model": "writer",
                    "prompt": {"text": "fixture " + "x" * (512 * 1024)},
                },
                "outputs": {"answer": {"schema": schema}},
            }
        target, expected, calls, schema = (
            "nodes.switch_0999.outputs.route",
            "done",
            4,
            {"type": "string"},
        )
    elif name == "nested_foreach":
        items = {"type": "array", "items": {"type": "integer"}}
        leaf = {
            "type": "llm",
            "inputs": {
                key: {"schema": schema, "bind": {"from": "inputs." + key}}
                for key in ("benchmark_row", "benchmark_column")
            },
            "llm": {"model": "writer", "prompt": {"text": "Return row * 8 + column."}},
            "outputs": {"answer": {"schema": schema}},
        }
        inner = {
            "type": "foreach",
            "inputs": {
                "columns": {"schema": items, "bind": {"value": list(range(8))}},
                "benchmark_row": {
                    "schema": schema,
                    "bind": {"from": "inputs.benchmark_row"},
                },
            },
            "foreach": {
                "over": "columns",
                "concurrency": 4,
                "with": {
                    "benchmark_row": {"from": "args.benchmark_row"},
                    "benchmark_column": {"from": "iteration.item"},
                },
                "body": {
                    "inputs": {
                        key: {"schema": schema}
                        for key in ("benchmark_row", "benchmark_column")
                    },
                    "nodes": {"answer": leaf},
                    "outputs": {
                        "answer": {
                            "schema": schema,
                            "bind": {"from": "nodes.answer.outputs.answer"},
                        }
                    },
                },
            },
        }
        nodes["rows"] = {
            "type": "foreach",
            "inputs": {"rows": {"schema": items, "bind": {"value": list(range(8))}}},
            "foreach": {
                "over": "rows",
                "concurrency": 4,
                "with": {"benchmark_row": {"from": "iteration.item"}},
                "body": {
                    "inputs": {"benchmark_row": {"schema": schema}},
                    "nodes": {"columns": inner},
                    "outputs": {
                        "answer": {
                            "schema": items,
                            "bind": {"from": "nodes.columns.outputs.answer"},
                        }
                    },
                },
            },
        }
        target, expected, calls, schema = (
            "nodes.rows.outputs.answer",
            [[row * 8 + column for column in range(8)] for row in range(8)],
            64,
            {"type": "array", "items": items},
        )
    elif name == "long_agent":
        models["writer"]["requires"] = ["structuredOutput", "toolCalling"]
        nodes["agent"] = {
            "type": "agent",
            "sandbox": "work",
            "tools": {"mcp": {"metrics": ["measure"]}},
            "agent": {
                "model": "writer",
                "maxSteps": 33,
                "prompt": {
                    "text": "Measure values 0 through 31, then return their sum."
                },
            },
            "outputs": {"answer": {"schema": schema}},
        }
        target, expected, calls = "nodes.agent.outputs.answer", 496, 33
    elif name in ("human_wait", "human_resume", "human_timeout", "human_recovery"):
        models = {}
        schema = {"type": "boolean"}
        for index in range(1 if name == "human_timeout" else 32):
            nodes[f"wait_{index}"] = {
                "type": "human",
                "human": {"prompt": {"text": "Wait for operator"}},
                "outputs": {"approved": {"schema": schema}},
            }
            if name == "human_timeout":
                nodes[f"wait_{index}"]["execution"] = {"timeout": "10s"}
        target, expected, calls = (
            "nodes.wait_0.outputs.approved",
            True if name in ("human_resume", "human_recovery") else None,
            0,
        )
    else:
        raise ValueError(f"unknown workload: {name}")
    pipeline = {
        "apiVersion": "knotra/v1",
        "kind": "Pipeline",
        "metadata": {"name": name.replace("_", "-")},
        "spec": {
            "models": models,
            "limits": {
                "timeout": "10m",
                "maxConcurrentNodes": 16,
                "maxNodeInstances": 4096,
            },
            "nodes": nodes,
            "outputs": {"result": {"schema": schema, "bind": {"from": target}}},
        },
    }
    if name == "long_agent":
        pipeline["spec"]["sandboxes"] = {"work": {"profile": "python"}}
        pipeline["spec"]["mcp"] = {
            "metrics": {"connection": "metrics", "session": "node"}
        }
    return pipeline, expected, calls, 73 if name == "nested_foreach" else len(nodes)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--binary", type=pathlib.Path, default=pathlib.Path("bin/knotra")
    )
    parser.add_argument(
        "--database-url",
        default=os.environ.get("KNOTRA_TEST_DATABASE_URL"),
        required=not os.environ.get("KNOTRA_TEST_DATABASE_URL"),
    )
    parser.add_argument("--postgres-container", required=True)
    parser.add_argument("--temporal-container", required=True)
    parser.add_argument("--temporal-address", required=True)
    parser.add_argument("--db-user", default="knotra")
    parser.add_argument("--db-name", default="knotra")
    parser.add_argument("--repeats", type=int, default=3)
    parser.add_argument(
        "--profile-sql",
        action="store_true",
        help="capture statement counter deltas (disposable database only)",
    )
    parser.add_argument(
        "--backends",
        nargs="+",
        choices=("temporal", "river"),
        default=("temporal", "river"),
    )
    parser.add_argument(
        "--workloads",
        nargs="+",
        choices=(
            "short_dag",
            "large_controls",
            "nested_foreach",
            "long_agent",
            "human_wait",
            "human_resume",
            "human_timeout",
            "human_recovery",
            "queue_retention",
        ),
        default=("short_dag", "large_controls", "human_wait"),
    )
    parser.add_argument("--idle-seconds", type=int, default=15)
    parser.add_argument("--wait-seconds", type=int, default=15)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    args = parser.parse_args()
    if "queue_retention" in args.workloads and (
        args.backends != ["river"] or not args.profile_sql
    ):
        parser.error("queue_retention requires --backends river --profile-sql")
    if (
        os.name != "posix"
        or min(args.repeats, args.idle_seconds, args.wait_seconds) < 1
    ):
        parser.error("requires POSIX and positive repeat/observation counts")
    args.binary = args.binary.resolve()
    args.output = args.output.resolve()
    args.output.parent.mkdir(parents=True, exist_ok=True)
    work = args.output.parent / ("work-" + uuid.uuid4().hex)
    work.mkdir()
    docker_host = os.environ.get("DOCKER_HOST") or command(
        "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"
    )
    if not docker_host.startswith("unix://"):
        parser.error("requires a local Unix Docker socket")
    docker_path = urllib.parse.urlparse(docker_host).path
    lock = threading.Lock()
    physical = []
    physical_tools = []

    class Provider(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_GET(self):
            if self.path == "/mcp":
                self.send_response(405)
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            data = b'{"id":"fixture-model"}'
            self.send_response(200)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            if self.path == "/mcp":
                self.mcp_response(body)
                return
            answer, delay = 42, 0.01
            for item in body["input"]:
                content = item.get("content", "")
                prefix = "Knotra input context (data):\n"
                if isinstance(content, str) and content.startswith(prefix):
                    values = json.loads(content.removeprefix(prefix))["values"]
                    if "benchmark_row" in values:
                        row, column = (
                            values["benchmark_row"],
                            values["benchmark_column"],
                        )
                        answer = row * 8 + column
                        delay += (7 - column) * 0.003
            tool_outputs = [
                item
                for item in body["input"]
                if item.get("type") == "function_call_output"
            ]
            is_agent = any(
                tool["name"] == "knotra_finish" for tool in body.get("tools", [])
            )
            name, call_id, arguments = "knotra_output", "answer", {"answer": answer}
            if is_agent:
                values = []
                for index, item in enumerate(tool_outputs):
                    assert item["call_id"] == f"call_{index}"
                    value = json.loads(item["output"])["structuredContent"]["value"]
                    assert value == index
                    values.append(value)
                step = len(values)
                assert step <= 32
                measure_tools = [
                    tool["name"]
                    for tool in body["tools"]
                    if tool.get("description", "").startswith("metrics.measure: ")
                ]
                assert len(measure_tools) == 1
                name = measure_tools[0] if step < 32 else "knotra_finish"
                call_id = str(step)
                arguments = {"value": step} if step < 32 else {"answer": sum(values)}
                delay = 1
            with lock:
                physical.append(time.monotonic())
            time.sleep(delay)
            data = json.dumps(
                {
                    "status": "completed",
                    "output": [
                        {
                            "type": "function_call",
                            "id": "fc_" + call_id,
                            "call_id": "call_" + call_id,
                            "name": name,
                            "arguments": json.dumps(arguments),
                        }
                    ],
                }
            ).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def mcp_response(self, body):
            if "id" not in body:
                self.send_response(202)
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            schema = {
                "type": "object",
                "properties": {"value": {"type": "integer"}},
                "required": ["value"],
                "additionalProperties": False,
            }
            method = body["method"]
            response = {"jsonrpc": "2.0", "id": body["id"]}
            if method == "initialize":
                result = {
                    "protocolVersion": body["params"]["protocolVersion"],
                    "capabilities": {"tools": {}},
                    "serverInfo": {"name": "benchmark", "version": "1"},
                }
            elif method == "tools/list":
                result = {
                    "tools": [
                        {
                            "name": "measure",
                            "description": "Echo one measured value",
                            "inputSchema": schema,
                            "outputSchema": schema,
                        }
                    ]
                }
            elif method == "tools/call":
                assert body["params"]["name"] == "measure"
                value = body["params"]["arguments"]["value"]
                assert isinstance(value, int) and 0 <= value < 32
                with lock:
                    physical_tools.append(value)
                time.sleep(1)
                result = {
                    "content": [{"type": "text", "text": json.dumps({"value": value})}],
                    "structuredContent": {"value": value},
                }
            elif method == "ping":
                result = {}
            else:
                response["error"] = {"code": -32601, "message": "Method not found"}
            if "error" not in response:
                response["result"] = result
            data = json.dumps(response).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

    provider = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Provider)
    threading.Thread(target=provider.serve_forever, daemon=True).start()
    profile = {
        "apiVersion": "knotra/v1",
        "kind": "EngineProfile",
        "metadata": {"name": "benchmark"},
        "spec": {
            "models": {
                "cloud": {
                    "provider": "openai",
                    "model": "fixture-model",
                    "baseUrl": f"http://127.0.0.1:{provider.server_port}",
                    "auth": {"key": {"secretRef": "fixture"}},
                }
            },
            "secrets": {"fixture": {"env": "KNOTRA_BENCHMARK_KEY"}},
            "sandboxes": {
                "python": {
                    "image": "python:3.13-alpine",
                    "network": {"mode": "none"},
                    "allowedTools": [],
                    "resources": {
                        "cpu": 1,
                        "memoryMiB": 128,
                        "diskMiB": 32,
                        "pids": 32,
                    },
                }
            },
            "mcp": {
                "metrics": {
                    "transport": "streamable_http",
                    "url": f"http://127.0.0.1:{provider.server_port}/mcp",
                    "allowedTools": ["measure"],
                    "toolPolicies": {"measure": {"effect": "read"}},
                }
            },
            "limits": {
                "timeout": "10m",
                "maxConcurrentNodes": 16,
                "maxNodeInstances": 4096,
                "maxModelCalls": 4096,
                "maxToolCalls": 4096,
            },
        },
    }
    profile_path = work / "profile.json"
    profile_path.write_text(json.dumps(profile))
    report = {
        "complete": False,
        "created_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "platform": platform.platform(),
        "logical_cpus": os.cpu_count(),
        "docker": json.loads(
            command(
                "docker",
                "info",
                "--format",
                '{"cpus":{{.NCPU}},"memory_bytes":{{.MemTotal}},"server_version":{{json .ServerVersion}}}',
            )
        ),
        "postgres_version": sql(args, "SELECT version()"),
        "binary_sha256": hashlib.sha256(args.binary.read_bytes()).hexdigest(),
        "git_head": command("git", "rev-parse", "HEAD"),
        "git_dirty": bool(command("git", "status", "--porcelain")),
        "sample_interval_seconds": 1,
        "fixture_model_delay_seconds": 0.01,
        "fixture_nested_column_delay_seconds": 0.003,
        "fixture_agent_model_delay_seconds": 1,
        "fixture_agent_tool_delay_seconds": 1,
        "poll_interval_seconds": 0.25,
        "repeats": args.repeats,
        "profile_sql": args.profile_sql,
        "requested_backends": args.backends,
        "requested_workloads": args.workloads,
        "idle_seconds": args.idle_seconds,
        "human_wait_seconds": args.wait_seconds,
        "infrastructure_storage_paths": {
            args.postgres_container: sql(args, "SHOW data_directory"),
            args.temporal_container: "/home/temporal",
        },
        "work_directory": str(work),
        "agent_execution": (
            {
                "sandbox_image_id": command(
                    "docker",
                    "image",
                    "inspect",
                    "python:3.13-alpine",
                    "--format",
                    "{{.Id}}",
                ),
                "helper_sha256": hashlib.sha256(
                    pathlib.Path(
                        os.environ.get(
                            "KNOTRA_SANDBOX_HELPER", ".knotra/bin/sandbox-helper"
                        )
                    ).read_bytes()
                ).hexdigest(),
            }
            if "long_agent" in args.workloads
            else None
        ),
        "containers": {},
        "backends": [],
    }
    for name in (args.postgres_container, args.temporal_container):
        report["containers"][name] = json.loads(
            command(
                "docker",
                "inspect",
                "--format",
                '{"image":{{json .Config.Image}},"image_id":{{json .Image}},"memory_limit_bytes":{{.HostConfig.Memory}},"pids_limit":{{json .HostConfig.PidsLimit}}}',
                name,
            )
        )

    def write_report():
        temporary = args.output.with_suffix(".tmp")
        temporary.write_text(json.dumps(report, indent=2) + "\n")
        temporary.replace(args.output)

    try:
        for backend in args.backends:
            schema = "knotra_perf_" + uuid.uuid4().hex
            sql(args, "CREATE SCHEMA " + schema)
            parsed = urllib.parse.urlsplit(args.database_url)
            params = dict(urllib.parse.parse_qsl(parsed.query))
            params.update(search_path=schema, application_name=schema)
            dsn = urllib.parse.urlunsplit(
                parsed._replace(query=urllib.parse.urlencode(params))
            )
            data_dir = work / backend
            base = f"http://127.0.0.1:{free_port()}"
            log = (work / (backend + ".log")).open("w")
            environment = dict(
                os.environ, KNOTRA_DATABASE_URL=dsn, KNOTRA_BENCHMARK_KEY="fixture-key"
            )
            process_command = [
                str(args.binary),
                "serve",
                "--backend",
                backend,
                "--listen",
                base.removeprefix("http://"),
                "--profile",
                str(profile_path),
                "--data-dir",
                str(data_dir),
                "--temporal-address",
                args.temporal_address,
                "--execution-workers",
                "16",
            ]

            def start_process():
                return subprocess.Popen(
                    process_command, env=environment, stdout=log, stderr=log
                )

            def wait_ready():
                deadline = time.monotonic() + 60
                while True:
                    if process.poll() is not None:
                        raise RuntimeError(f"{backend} exited; inspect {log.name}")
                    try:
                        return request(base, "/info", timeout=1)["engineId"]
                    except (OSError, urllib.error.URLError):
                        if time.monotonic() > deadline:
                            raise TimeoutError(f"{backend} startup; inspect {log.name}")
                        time.sleep(0.1)

            process = start_process()
            process_lock = threading.Lock()
            engine_cpu_offset = 0
            current = {"backend": backend, "measurements": [], "sample_errors": []}
            report["backends"].append(current)
            containers = [args.postgres_container] + (
                [args.temporal_container] if backend == "temporal" else []
            )
            samples, stopped = [], threading.Event()

            def sample():
                with process_lock:
                    engine_stats = process_stats(process.pid)
                    engine_stats["cpu_seconds"] += engine_cpu_offset
                sandboxes = {}
                if "long_agent" in args.workloads:
                    for sandbox in command(
                        "docker",
                        "ps",
                        "--filter",
                        "label=io.knotra.engine=" + engine_id,
                        "--format",
                        "{{.ID}}",
                    ).splitlines():
                        try:
                            sandboxes[sandbox] = docker_stats(docker_path, sandbox)
                        except RuntimeError as error:
                            if not str(error).endswith("HTTP 404"):
                                raise
                return {
                    "time": time.monotonic(),
                    "engine": engine_stats,
                    "containers": {
                        name: docker_stats(docker_path, name) for name in containers
                    },
                    "database": database_stats(args, schema),
                    "sandboxes": sandboxes,
                }

            def monitor():
                while not stopped.is_set():
                    try:
                        samples.append(sample())
                    except (
                        OSError,
                        ValueError,
                        RuntimeError,
                        subprocess.CalledProcessError,
                    ) as error:
                        current["sample_errors"].append(str(error))
                    stopped.wait(1)

            monitor_thread = None
            try:
                engine_id = wait_ready()
                monitor_thread = threading.Thread(target=monitor)
                monitor_thread.start()
                stages = [("idle", 0)] + [
                    (name, repeat)
                    for name in args.workloads
                    for repeat in range(
                        1 if name in ("human_wait", "queue_retention") else args.repeats
                    )
                ]
                for name, repeat in stages:
                    print(f"{backend}: {name} {repeat + 1}", flush=True)
                    before = sample()
                    storage_before = {
                        container: allocated_storage(
                            container, report["infrastructure_storage_paths"][container]
                        )
                        for container in containers
                    }
                    statements_before = (
                        statement_stats(args) if args.profile_sql else {}
                    )
                    with lock:
                        call_start = len(physical)
                        tool_start = len(physical_tools)
                    started = time.monotonic()
                    record = {"workload": name, "repeat": repeat + 1}
                    if name == "idle":
                        time.sleep(args.idle_seconds)
                    elif name == "queue_retention":
                        assert (
                            int(sql(args, f"SELECT count(*) FROM {schema}.knotra_runs"))
                            > 0
                        )
                        snapshot_query = f"""SELECT md5(jsonb_build_object(
                            'runs',(SELECT jsonb_agg(document::jsonb ORDER BY id) FROM {schema}.knotra_runs),
                            'instances',(SELECT jsonb_agg(to_jsonb(i) ORDER BY run_id,id) FROM {schema}.knotra_instances i),
                            'attempts',(SELECT jsonb_agg(to_jsonb(a) ORDER BY run_id,instance_id,number) FROM {schema}.knotra_execution_attempts a),
                            'budgets',(SELECT jsonb_agg(to_jsonb(b) ORDER BY run_id,scope,kind) FROM {schema}.knotra_budgets b),
                            'events',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM {schema}.knotra_events e))::text)"""
                        domain_before = sql(args, snapshot_query)
                        seed_started = time.monotonic()
                        sql(
                            args,
                            f"""INSERT INTO {schema}.river_job(kind,args,queue,state,finalized_at,max_attempts,metadata)
                            SELECT 'retention_fixture','{{}}'::jsonb,'retention_fixture',
                                (ARRAY['completed','cancelled','discarded'])[1+n%3]::{schema}.river_job_state,
                                clock_timestamp()-interval '8 days',1,'{{"cohort":"expired"}}'::jsonb
                            FROM generate_series(1,10000) n;
                            INSERT INTO {schema}.river_job(kind,args,queue,state,finalized_at,max_attempts,scheduled_at,metadata)
                            SELECT 'retention_fixture','{{}}'::jsonb,'retention_fixture',state::{schema}.river_job_state,
                                CASE WHEN state IN ('completed','cancelled','discarded') THEN clock_timestamp() END,
                                1,clock_timestamp()+interval '1 day','{{"cohort":"protected"}}'::jsonb
                            FROM unnest(ARRAY['completed','cancelled','discarded','available','pending','scheduled']) state""",
                        )
                        record["seed_seconds"] = time.monotonic() - seed_started
                        deadline = time.monotonic() + 75
                        while int(
                            sql(
                                args,
                                f"SELECT count(*) FROM {schema}.river_job WHERE kind='retention_fixture' AND metadata->>'cohort'='expired'",
                            )
                        ):
                            if time.monotonic() > deadline:
                                raise TimeoutError(
                                    "River cleaner did not remove expired jobs"
                                )
                            time.sleep(0.25)
                        record["seed_to_observed_cleanup_seconds"] = (
                            time.monotonic() - seed_started
                        )
                        protected = json.loads(
                            sql(
                                args,
                                f"SELECT json_agg(state ORDER BY state) FROM {schema}.river_job WHERE kind='retention_fixture' AND metadata->>'cohort'='protected'",
                            )
                        )
                        assert sorted(protected) == sorted(
                            (
                                "completed",
                                "cancelled",
                                "discarded",
                                "available",
                                "pending",
                                "scheduled",
                            )
                        )
                        assert sql(args, snapshot_query) == domain_before
                        record.update(
                            expired_jobs=10000,
                            protected_jobs=6,
                            domain_snapshot_unchanged=True,
                            includes_seed_and_wait=True,
                        )
                        finished = time.monotonic()
                        # Let PostgreSQL flush the cleaner connection's statement counters.
                        time.sleep(1)
                    else:
                        pipeline, expected, expected_calls, node_count = workload(name)
                        source = json.dumps(pipeline, separators=(",", ":"))
                        package = {
                            "entrypoint": "pipeline.yaml",
                            "source": source,
                            "files": [
                                {
                                    "path": "pipeline.yaml",
                                    "content": base64.b64encode(
                                        source.encode()
                                    ).decode(),
                                }
                            ],
                        }
                        definition = request(
                            base, "/definitions", {"package": package}
                        )["definition"]["id"]
                        if backend == "river":
                            record["advance_backlog_before_admission"] = json.loads(
                                sql(
                                    args,
                                    f"""
                                    SELECT COALESCE(json_agg(backlog),'[]') FROM (
                                        SELECT r.document->>'status' AS run_status,j.state,count(*) AS jobs
                                        FROM {schema}.river_job j JOIN {schema}.knotra_runs r ON r.id=j.args->>'runId'
                                        WHERE j.kind='knotra_advance_v1' AND j.state IN ('available','running','retryable','scheduled') GROUP BY 1,2
                                    ) backlog
                                """,
                                )
                            )
                        admit_started = time.monotonic()
                        run_id = request(
                            base,
                            "/runs",
                            {"definitionId": definition, "profile": "benchmark"},
                        )["run"]["id"]
                        record["admission_ms"] = (
                            time.monotonic() - admit_started
                        ) * 1000
                        if name in (
                            "human_wait",
                            "human_resume",
                            "human_timeout",
                            "human_recovery",
                        ):
                            deadline = time.monotonic() + 60
                            while True:
                                count = int(
                                    sql(
                                        args,
                                        f"SELECT count(*) FROM {schema}.knotra_requests WHERE run_id='{run_id}' AND status='open'",
                                    )
                                )
                                if count == node_count:
                                    break
                                if time.monotonic() > deadline:
                                    raise TimeoutError("human wait materialization")
                                time.sleep(0.25)
                            if name == "human_timeout":
                                run = wait_run(base, run_id, "failed")
                                assert any(
                                    diagnostic["code"] == "DEADLINE_EXCEEDED"
                                    for diagnostic in run["diagnostics"]
                                )
                            else:
                                time.sleep(args.wait_seconds)
                                if name == "human_recovery":
                                    snapshot_query = f"""SELECT json_agg(json_build_object(
                                        'id',id,'instance_id',document->>'instanceId',
                                        'deadline',document->>'deadline','status',status)
                                        ORDER BY id) FROM {schema}.knotra_requests WHERE run_id='{run_id}'"""
                                    original_requests = json.loads(
                                        sql(args, snapshot_query)
                                    )
                                    assert len(original_requests) == node_count
                                    original_run = request(base, "/runs/" + run_id)[
                                        "run"
                                    ]
                                    with process_lock:
                                        engine_cpu_offset += process_stats(process.pid)[
                                            "cpu_seconds"
                                        ]
                                        old_pid = process.pid
                                        recovery_started = time.monotonic()
                                        process.kill()
                                        assert (
                                            process.wait(timeout=15) == -signal.SIGKILL
                                        )
                                        process = start_process()
                                        assert process.pid != old_pid
                                        assert wait_ready() == engine_id
                                        record["sigkill_to_api_ready_seconds"] = (
                                            time.monotonic() - recovery_started
                                        )
                                    assert (
                                        json.loads(sql(args, snapshot_query))
                                        == original_requests
                                    )
                                    recovered_run = request(base, "/runs/" + run_id)[
                                        "run"
                                    ]
                                    assert recovered_run["id"] == original_run["id"]
                                    assert {
                                        n["id"] for n in recovered_run["instances"]
                                    } == {n["id"] for n in original_run["instances"]}
                                    record["preserved_requests"] = len(
                                        original_requests
                                    )
                                    record["engine_cpu_is_lower_bound"] = True
                                if name == "human_wait":
                                    request(base, "/runs/" + run_id + "/cancel", {})
                                    run = wait_run(base, run_id, "cancelled")
                                else:
                                    pending = json.loads(
                                        sql(
                                            args,
                                            f"SELECT json_agg(id ORDER BY id) FROM {schema}.knotra_requests WHERE run_id='{run_id}' AND status='open'",
                                        )
                                    )
                                    assert len(pending) == node_count
                                    resumed = time.monotonic()
                                    for request_id in pending:
                                        request(
                                            base,
                                            "/requests/" + request_id + "/response",
                                            {"outputs": {"approved": True}},
                                        )
                                    record["response_commands_seconds"] = (
                                        time.monotonic() - resumed
                                    )
                                    run = wait_run(base, run_id, "succeeded")
                                    record["responses_to_terminal_seconds"] = (
                                        time.monotonic() - resumed
                                    )
                                    assert run["outputs"]["result"] is True
                                    assert all(
                                        node["outputs"]["values"]["approved"] is True
                                        for node in run["instances"]
                                    )
                            if name in (
                                "human_resume",
                                "human_timeout",
                                "human_recovery",
                            ):
                                origin = (
                                    "q.accepted_at"
                                    if name in ("human_resume", "human_recovery")
                                    else "(q.document->>'deadline')::timestamptz"
                                )
                                record["human_completion_timings"] = json.loads(
                                    sql(
                                        args,
                                        f"""SELECT json_build_object(
                                            'count',count(lag),
                                            'minimum_ms',min(lag),
                                            'p50_ms',percentile_cont(0.5) WITHIN GROUP(ORDER BY lag),
                                            'p95_ms',percentile_cont(0.95) WITHIN GROUP(ORDER BY lag),
                                            'maximum_ms',max(lag)) FROM (
                                            SELECT extract(epoch FROM ((i.document->>'finishedAt')::timestamptz-{origin}))*1000 AS lag
                                            FROM {schema}.knotra_requests q JOIN {schema}.knotra_instances i
                                            ON i.run_id=q.run_id AND i.id=q.document->>'instanceId'
                                            WHERE q.run_id='{run_id}'
                                        ) timings""",
                                    )
                                )
                                timing = record["human_completion_timings"]
                                assert timing["count"] == node_count
                                assert timing["minimum_ms"] is not None
                                if name == "human_timeout":
                                    assert timing["minimum_ms"] >= 0
                                    if backend == "river":
                                        record["due_timer_consumption_ms"] = json.loads(
                                            sql(
                                                args,
                                                f"SELECT json_agg(extract(epoch FROM consumed_at-due_at)*1000) FROM {schema}.knotra_execution_timers WHERE run_id='{run_id}' AND kind='node_deadline' AND consumed_at IS NOT NULL",
                                            )
                                        )
                                        assert (
                                            len(record["due_timer_consumption_ms"]) == 1
                                        )
                                        assert (
                                            record["due_timer_consumption_ms"][0] >= 0
                                        )
                        else:
                            run = wait_run(base, run_id, "succeeded")
                            if run["outputs"]["result"] != expected:
                                raise AssertionError("full run export changed")
                        finished = time.monotonic()
                        record["admission_to_terminal_seconds"] = (
                            finished - admit_started
                        )
                        with lock:
                            times = physical[call_start:]
                            tool_values = physical_tools[tool_start:]
                        if len(times) != expected_calls:
                            raise AssertionError(
                                f"{name}: physical calls={len(times)}, want {expected_calls}"
                            )
                        budget = int(
                            sql(
                                args,
                                f"SELECT COALESCE(sum(used),0) FROM {schema}.knotra_budgets WHERE run_id='{run_id}' AND scope='{run_id}' AND kind='model'",
                            )
                        )
                        if (
                            budget != expected_calls
                            or len(run["instances"]) != node_count
                        ):
                            raise AssertionError(
                                f"{name}: budget={budget}, nodes={len(run['instances'])}, want {expected_calls}/{node_count}"
                            )
                        tool_budget = int(
                            sql(
                                args,
                                f"SELECT COALESCE(sum(used),0) FROM {schema}.knotra_budgets WHERE run_id='{run_id}' AND scope='{run_id}' AND kind='tool'",
                            )
                        )
                        expected_tools = 32 if name == "long_agent" else 0
                        if tool_budget != expected_tools or tool_values != (
                            list(range(32)) if name == "long_agent" else []
                        ):
                            raise AssertionError(
                                f"{name}: tool debits={tool_budget}, physical values={tool_values}"
                            )
                        if name == "long_agent" and not run["instances"][0][
                            "attemptId"
                        ].endswith(".a1"):
                            raise AssertionError("long agent changed attempt")
                        record.update(
                            run_id=run_id,
                            nodes=node_count,
                            physical_model_calls=len(times),
                            model_debits=budget,
                            physical_tool_calls=len(tool_values),
                            tool_debits=tool_budget,
                            first_physical_call_ms=(
                                (times[0] - admit_started) * 1000 if times else None
                            ),
                        )
                        if backend == "river":
                            record["delivery_timings"] = json.loads(
                                sql(
                                    args,
                                    f"""
                                SELECT json_build_object(
                                    'first_node_start_ms',(SELECT extract(epoch FROM min(n.started_at)-r.admitted_at)*1000 FROM {schema}.knotra_execution_nodes n WHERE n.run_id=r.id),
                                    'queues',(SELECT json_object_agg(kind,summary) FROM (
                                        SELECT kind,json_build_object(
                                            'jobs',count(*),
                                            'maximum_attempt',max(attempt),
                                            'first_start_ms',extract(epoch FROM min(attempted_at)-r.admitted_at)*1000,
                                            'queue_wait_p50_ms',percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM attempted_at-created_at)*1000),
                                            'queue_wait_p95_ms',percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM attempted_at-created_at)*1000),
                                            'queue_wait_max_ms',max(extract(epoch FROM attempted_at-created_at)*1000)
                                        ) AS summary FROM {schema}.river_job
                                        WHERE COALESCE(args->>'runId',args->'attempt'->>'runId')=r.id GROUP BY kind
                                    ) jobs)
                                ) FROM {schema}.knotra_runs r WHERE r.id='{run_id}'
                            """,
                                )
                            )
                    if name == "idle":
                        finished = time.monotonic()
                    after = sample()
                    storage_after = {
                        container: allocated_storage(
                            container, report["infrastructure_storage_paths"][container]
                        )
                        for container in containers
                    }
                    if args.profile_sql:
                        deltas = []
                        for query_id, row in statement_stats(args).items():
                            previous = statements_before.get(query_id, {})
                            delta = {
                                key: row[key] - previous.get(key, 0)
                                for key in (
                                    "calls",
                                    "exec_ms",
                                    "wal_bytes",
                                    "wal_records",
                                    "wal_fpi",
                                )
                            }
                            if delta["calls"]:
                                deltas.append(
                                    {"id": query_id, "query": row["query"], **delta}
                                )
                        record["statement_deltas"] = sorted(
                            deltas, key=lambda row: row["wal_bytes"], reverse=True
                        )
                        if name == "queue_retention":
                            record["cleaner_statement_deltas"] = [
                                row
                                for row in deltas
                                if row["query"].startswith(
                                    f'DELETE FROM "{schema}".river_job'
                                )
                                and "finalized_at <" in row["query"]
                            ]
                            assert record["cleaner_statement_deltas"]
                    window = (
                        [before]
                        + [
                            item
                            for item in samples
                            if before["time"] <= item["time"] <= after["time"]
                        ]
                        + [after]
                    )
                    elapsed = finished - started
                    sandbox_cpu = {}
                    for item in window:
                        for sandbox, stats in item["sandboxes"].items():
                            sandbox_cpu[sandbox] = max(
                                sandbox_cpu.get(sandbox, 0), stats["cpu_seconds"]
                            )
                    sandbox_cpu_delta = sum(
                        cpu - before["sandboxes"].get(sandbox, {}).get("cpu_seconds", 0)
                        for sandbox, cpu in sandbox_cpu.items()
                    )
                    if name == "long_agent" and len(sandbox_cpu) != 1:
                        raise AssertionError(
                            f"long agent used {len(sandbox_cpu)} observed sandboxes"
                        )
                    record.update(
                        elapsed_seconds=elapsed,
                        samples=len(window),
                        maximum_sample_gap_seconds=max(
                            right["time"] - left["time"]
                            for left, right in zip(window, window[1:])
                        ),
                        deployment_peak_memory_bytes=max(
                            item["engine"]["rss_bytes"]
                            + sum(
                                value["working_set_bytes"]
                                for value in item["containers"].values()
                            )
                            + sum(
                                value["working_set_bytes"]
                                for value in item["sandboxes"].values()
                            )
                            for item in window
                        ),
                        deployment_cpu_seconds=after["engine"]["cpu_seconds"]
                        - before["engine"]["cpu_seconds"]
                        + sandbox_cpu_delta
                        + sum(
                            after["containers"][container]["cpu_seconds"]
                            - before["containers"][container]["cpu_seconds"]
                            for container in containers
                        ),
                        engine_peak_rss_bytes=max(
                            item["engine"]["rss_bytes"] for item in window
                        ),
                        sandbox_peak_working_set_bytes=max(
                            sum(
                                stats["working_set_bytes"]
                                for stats in item["sandboxes"].values()
                            )
                            for item in window
                        ),
                        sandbox_cpu_seconds_lower_bound=sandbox_cpu_delta,
                        observed_sandboxes=len(sandbox_cpu),
                        engine_cpu_seconds=after["engine"]["cpu_seconds"]
                        - before["engine"]["cpu_seconds"],
                        database_connection_peak=max(
                            item["database"]["connections"] for item in window
                        ),
                        storage_bytes=sum(
                            path.stat().st_size
                            for path in data_dir.rglob("*")
                            if path.is_file()
                        ),
                        database_deltas={
                            key: after["database"][key] - before["database"][key]
                            for key in (
                                "wal_bytes",
                                "query_calls",
                                "query_exec_ms",
                                "transactions",
                                "schema_bytes",
                            )
                        },
                        infrastructure={
                            container: {
                                "peak_working_set_bytes": max(
                                    item["containers"][container]["working_set_bytes"]
                                    for item in window
                                ),
                                "cpu_seconds": after["containers"][container][
                                    "cpu_seconds"
                                ]
                                - before["containers"][container]["cpu_seconds"],
                                "allocated_storage_before_bytes": storage_before[
                                    container
                                ],
                                "allocated_storage_after_bytes": storage_after[
                                    container
                                ],
                                "allocated_storage_growth_bytes": storage_after[
                                    container
                                ]
                                - storage_before[container],
                            }
                            for container in containers
                        },
                    )
                    if name in ("short_dag", "large_controls", "nested_foreach"):
                        record["nodes_per_second"] = node_count / elapsed
                    current["measurements"].append(record)
                    write_report()
                    print(
                        f"  {elapsed:.3f}s, engine peak RSS {record['engine_peak_rss_bytes'] / 2**20:.1f} MiB",
                        flush=True,
                    )
                if current["sample_errors"]:
                    raise RuntimeError(
                        f"resource samples failed: {current['sample_errors']}"
                    )
            finally:
                stopped.set()
                if monitor_thread is not None:
                    monitor_thread.join(timeout=15)
                process.send_signal(signal.SIGINT) if process.poll() is None else None
                try:
                    process.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
                log.close()
                sql(args, "DROP SCHEMA " + schema + " CASCADE")
                write_report()
        report["complete"] = True
        report["completed_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    finally:
        provider.shutdown()
        provider.server_close()
        write_report()
    print(args.output, flush=True)


if __name__ == "__main__":
    main()
