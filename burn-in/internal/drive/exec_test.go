package drive

import (
	"testing"
)

func TestSplitOnCRLF(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"newline", "hello\nworld", []string{"hello", "world"}},
		{"cr", "hello\rworld", []string{"hello", "world"}},
		{"crlf", "hello\r\nworld", []string{"hello", "world"}},
		{"mixed", "a\rb\nc\r\nd", []string{"a", "b", "c", "d"}},
		{"trailing", "hello\n", []string{"hello"}},
		{"empty_lines", "a\n\nb", []string{"a", "b"}},
		{"single", "hello", []string{"hello"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			data := []byte(tt.input)
			for len(data) > 0 {
				advance, token, _ := splitOnCRLF(data, false)
				if advance == 0 {
					// Need more data — simulate EOF.
					_, token, _ = splitOnCRLF(data, true)
					if token != nil {
						got = append(got, string(token))
					}
					break
				}
				if token != nil {
					got = append(got, string(token))
				}
				data = data[advance:]
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d tokens %v, want %d tokens %v", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("token[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestInferPartitionPath(t *testing.T) {
	tests := []struct {
		device  string
		partNum int
		want    string
	}{
		{"/dev/sda", 1, "/dev/sda1"},
		{"/dev/sdb", 2, "/dev/sdb2"},
		{"/dev/nvme0n1", 1, "/dev/nvme0n1p1"},
		{"/dev/loop0", 1, "/dev/loop0p1"},
	}

	for _, tt := range tests {
		got := inferPartitionPath(tt.device, tt.partNum)
		if got != tt.want {
			t.Errorf("inferPartitionPath(%q, %d) = %q, want %q", tt.device, tt.partNum, got, tt.want)
		}
	}
}

func TestInferDeviceFromPartition(t *testing.T) {
	tests := []struct {
		partition string
		want      string
	}{
		{"/dev/sda1", "/dev/sda"},
		{"/dev/sdb2", "/dev/sdb"},
		{"/dev/nvme0n1p1", "/dev/nvme0n1"},
		{"/dev/loop0p1", "/dev/loop0"},
	}

	for _, tt := range tests {
		got := inferDeviceFromPartition(tt.partition)
		if got != tt.want {
			t.Errorf("inferDeviceFromPartition(%q) = %q, want %q", tt.partition, got, tt.want)
		}
	}
}

func TestSafeDeviceName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/dev/sda", "sda"},
		{"/dev/nvme0n1", "nvme0n1"},
		{"/dev/disk/by-id/foo:bar", "foo_bar"},
	}

	for _, tt := range tests {
		got := SafeDeviceName(tt.input)
		if got != tt.want {
			t.Errorf("SafeDeviceName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
