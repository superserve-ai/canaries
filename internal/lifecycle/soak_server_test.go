package lifecycle

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSoakServerDetachedProcessAndMemoryIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("server launch uses Unix process sessions")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	script := filepath.Join(t.TempDir(), "server.py")
	if err := os.WriteFile(script, soakServerScript, 0o600); err != nil {
		t.Fatal(err)
	}
	// Exercise the actual server after its launcher exits. Both listeners must
	// expose one in-memory identity, and a new process must get a new identity.
	check := `import json,os,signal,socket,subprocess,sys,time,urllib.request
pids=[]
def launch():
 sockets=[socket.socket(),socket.socket()]
 for sock in sockets: sock.bind(("127.0.0.1",0))
 ports=[sock.getsockname()[1] for sock in sockets]
 for sock in sockets: sock.close()
 launcher="import subprocess,sys; p=subprocess.Popen([sys.executable,*sys.argv[1:]],stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,start_new_session=True); print(p.pid)"
 result=subprocess.run([sys.executable,"-c",launcher,sys.argv[1],*[str(port) for port in ports]],capture_output=True,text=True,check=True,timeout=5)
 pid=int(result.stdout.strip()); pids.append(pid)
 for attempt in range(100):
  try:
   responses=[]
   for port in ports:
    with urllib.request.urlopen("http://127.0.0.1:%d/?challenge=fresh"%port,timeout=1) as response:
     assert response.headers["Cache-Control"]=="no-store"
     responses.append(json.load(response))
   assert responses[0]==responses[1]
   assert responses[0]["pid"]==pid and responses[0]["challenge"]=="fresh"
   assert responses[0]["instance_id"] and responses[0]["started_at"]
   return responses[0]
  except OSError:
   if attempt==99: raise
   time.sleep(0.02)
try:
 first=launch(); second=launch()
 assert first["instance_id"]!=second["instance_id"], "replacement reused process identity"
 assert first["pid"]!=second["pid"]
finally:
 for pid in pids:
  try: os.kill(pid,signal.SIGTERM)
  except ProcessLookupError: pass
`
	cmd := exec.Command(python, "-c", check, script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("soak server check failed: %v\n%s", err, out)
	}
}
