package codexapp

import (
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// A child that sends many server requests without reading the answers, then answers a call, gets that answer
// through: the reader never waits on the child's input, so the child never blocks on its full output.
func TestServerRequestsDoNotStallTheReader(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	// 3000 requests make ~250 KiB of refusals and ~400 KiB of requests, both far beyond a pipe buffer. The child
	// reads the call, floods, answers, and only then reads its input.
	script := fakeCodex(t, "call = json.loads(sys.stdin.readline())\n"+
		"for i in range(3000):\n"+
		"    sys.stdout.write(json.dumps({'id': 'srv-%d' % i, 'method': 'item/approve', 'params': {'pad': 'x' * 100}}) + '\\n')\n"+
		"sys.stdout.write(json.dumps({'id': call['id'], 'result': {'ok': True}}) + '\\n')\n"+
		"sys.stdout.flush()\n"+
		"n = 0\n"+
		"for line in sys.stdin:\n"+
		"    if json.loads(line).get('error', {}).get('code') == -32601: n += 1\n"+
		"    if n == 3000: break\n"+
		"sys.stderr.write('refusals %d\\n' % n)\n"+
		"time.sleep(30)\n")
	c, err := Start(t.Context(), Config{Bin: script, Env: EnvOf([]string{"PATH=/usr/bin:/bin"}), Timeout: 8 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var out struct{ OK bool }
	if err := c.Call(t.Context(), "echo", nil, &out); err != nil || !out.OK {
		t.Fatalf("call: %v %v", err, out)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("the call took %v behind the refusals", d)
	}
	// Every refusal reaches the child, in order behind the reply it was waiting to send.
	deadline := time.Now().Add(8 * time.Second)
	for !strings.Contains(c.Stderr(), "refusals 3000") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(c.Stderr(), "refusals 3000") {
		t.Errorf("the child did not read 3000 refusals: %q", c.Stderr())
	}
	_ = c.Close()
}

// A child that sends server requests without ever reading the answers fails the client once the queued refusals pass
// their bound, instead of growing without limit.
func TestUnreadRefusalsAreBounded(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	// Each id is 1 MiB, so 12 requests queue about 12 MiB of refusals beyond what the pipe holds.
	script := fakeCodex(t, "call = json.loads(sys.stdin.readline())\n"+
		"for i in range(12):\n"+
		"    sys.stdout.write(json.dumps({'id': 'x' * (1 << 20) + str(i), 'method': 'item/approve'}) + '\\n')\n"+
		"sys.stdout.flush()\n"+
		"time.sleep(30)\n")
	c, err := Start(t.Context(), Config{Bin: script, Env: EnvOf([]string{"PATH=/usr/bin:/bin"}), Timeout: 8 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = c.Call(t.Context(), "echo", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "without reading the answers") {
		t.Fatalf("call: %v", err)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("the overflow took %v to fail the call", d)
	}
	_ = c.Close()
}
