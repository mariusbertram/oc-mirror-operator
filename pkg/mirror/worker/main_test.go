package worker

import (
	"context"
	"errors"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name      string
		batch     string
		src, dest string
		copyErr   bool
		want      int
	}{
		{name: "invalid batch", batch: "{", want: 1},
		{name: "empty batch", batch: "[]", want: 0},
		{name: "batch with failures still exits 0", batch: `[{"source":"s","dest":"reg/0"}]`, copyErr: true, want: 0},
		{name: "no batch and no flags", want: 1},
		{name: "legacy single image", src: "s", dest: "reg/0", want: 0},
		{name: "legacy single image failure", src: "s", dest: "reg/0", copyErr: true, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if tt.copyErr {
				h.copyErrs["reg/0"] = []error{errors.New("x"), errors.New("x")}
			}
			if got := run(context.Background(), h.w, tt.batch, tt.src, tt.dest); got != tt.want {
				t.Fatalf("run = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMain_Flags(t *testing.T) {
	if got := Main([]string{"--no-such-flag"}); got != 1 {
		t.Fatalf("bad flag: %d", got)
	}
	t.Setenv("MIRROR_BATCH", "[]")
	t.Setenv("MANAGER_URL", "")
	if got := Main([]string{"--insecure"}); got != 0 {
		t.Fatalf("empty batch: %d", got)
	}
}
