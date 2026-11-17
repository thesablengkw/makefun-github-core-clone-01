import { spawn, type ChildProcess } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { accessSync, constants } from "node:fs";

// Thin launcher for the vendored forge binary (statically linked Linux amd64 C,
// no runtime dependencies). Lifecycle contract with the harness: the binary binds an
// ephemeral port, POSTs it to $MAKEFUN_ENV_URL/register, serves until GET /done
// answers {"done": true}, then exits 0. This launcher only forwards signals and the
// exit code; it never interprets traffic.

const dir = dirname(fileURLToPath(import.meta.url));
const bin = join(dir, "forge");

// Fail fast with an actionable message instead of a bare spawn ENOENT when the
// binary is missing or not executable (e.g. a partial checkout).
try {
  accessSync(bin, constants.X_OK);
} catch {
  console.error(`forge binary missing or not executable: ${bin}`);
  process.exit(1);
}

const child: ChildProcess = spawn(bin, [], { stdio: "inherit" });

const forward = (signal: NodeJS.Signals): void => {
  if (!child.kill(signal)) process.exit(1);
};
process.on("SIGINT", () => forward("SIGINT"));
process.on("SIGTERM", () => forward("SIGTERM"));

child.on("error", (err) => {
  console.error(`failed to start forge binary: ${err.message}`);
  process.exit(1);
});
child.on("exit", (code, signal) => {
  if (signal) process.kill(process.pid, signal);
  else process.exit(code ?? 1);
});
