// Launcher: the platform runs `node /bot/main.ts`; this file starts the
// vendored static forge binary next to it and forwards the exit code.
//
// Mode-proofing (S3): the forge binary can land in the pod without its
// executable bit (a web upload once recorded it as mode 100644), which
// made the spawn fail with EACCES and the server never started. Before
// spawning, restore the bit in place; if the file is still not
// executable afterwards (e.g. a read-only directory), run a verified
// executable copy from a fresh writable temp directory instead.
import {
  spawn,
  type ChildProcess,
} from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  accessSync,
  chmodSync,
  constants,
  copyFileSync,
  mkdtempSync,
} from "node:fs";
import { tmpdir } from "node:os";

const dir = dirname(fileURLToPath(import.meta.url));

function isExecutable(path: string): boolean {
  try {
    accessSync(path, constants.X_OK);
    return true;
  } catch {
    return false;
  }
}

function resolveForgePath(): string {
  const forgePath = join(dir, "forge");
  try {
    chmodSync(forgePath, 0o755);
  } catch {
    // In-place chmod failed (e.g. read-only directory): fall through to
    // the temp-copy path below.
  }
  if (isExecutable(forgePath)) {
    return forgePath;
  }
  const copyDir = mkdtempSync(join(tmpdir(), "forge-"));
  const copyPath = join(copyDir, "forge");
  copyFileSync(forgePath, copyPath);
  chmodSync(copyPath, 0o755);
  if (!isExecutable(copyPath)) {
    throw new Error(`forge copy at ${copyPath} is not executable`);
  }
  return copyPath;
}

let forgeBin: string;
try {
  forgeBin = resolveForgePath();
} catch (err) {
  // Same failure surface as a spawn error: message on stderr, exit 1.
  console.error(
    `failed to start forge: ${err instanceof Error ? err.message : String(err)}`,
  );
  process.exit(1);
}
const child: ChildProcess = spawn(forgeBin, process.argv.slice(2), {
  stdio: "inherit",
  env: process.env,
});
child.on("error", (err) => {
  console.error(`failed to start forge: ${err.message}`);
  process.exit(1);
});
// Forward termination signals so the forge shuts down cleanly instead of
// being left orphaned when the platform stops this launcher.
for (const signal of ["SIGINT", "SIGTERM"] as const) {
  process.on(signal, () => child.kill(signal));
}
child.on("exit", (code) => process.exit(code ?? 1));
