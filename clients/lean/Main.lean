import DaisugiVerify.Wire

/-- Wire loop: one case JSON per line on stdin, one verdict JSON per line on
stdout, flushed per line. `handleLine` never throws, so every line gets a
verdict and the process only exits non-zero on a genuine process-level
failure (per docs/spec/conformance.md). -/
partial def loop (stdin : IO.FS.Stream) (stdout : IO.FS.Stream) : IO Unit := do
  let line ← stdin.getLine
  if line.isEmpty then
    pure ()
  else
    let trimmed := DaisugiVerify.pyStrip line
    if !trimmed.isEmpty then
      stdout.putStrLn (DaisugiVerify.handleLine trimmed)
      stdout.flush
    loop stdin stdout

def main : IO Unit := do
  let stdin ← IO.getStdin
  let stdout ← IO.getStdout
  loop stdin stdout
