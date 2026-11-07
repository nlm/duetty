package audioio

import "testing"

func TestMatchDeviceIndexByNumericIndex(t *testing.T) {
	names := []string{"Built-in Microphone", "USB Headset", "Bluetooth Mic"}

	idx, err := matchDeviceIndex(names, "1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idx != 1 {
		t.Errorf("idx = %d, want 1", idx)
	}
}

func TestMatchDeviceIndexByNumericIndexOutOfRange(t *testing.T) {
	names := []string{"Built-in Microphone", "USB Headset"}

	if _, err := matchDeviceIndex(names, "2"); err == nil {
		t.Fatal("expected an out-of-range error, got nil")
	}
	if _, err := matchDeviceIndex(names, "-1"); err == nil {
		t.Fatal("expected an out-of-range error for a negative index, got nil")
	}
}

func TestMatchDeviceIndexByNameSubstringIsCaseInsensitive(t *testing.T) {
	names := []string{"Built-in Microphone", "USB Headset", "Bluetooth Mic"}

	idx, err := matchDeviceIndex(names, "usb")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idx != 1 {
		t.Errorf("idx = %d, want 1", idx)
	}
}

func TestMatchDeviceIndexFirstSubstringMatchWins(t *testing.T) {
	names := []string{"Mic A", "Mic B", "Mic C"}

	idx, err := matchDeviceIndex(names, "mic")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idx != 0 {
		t.Errorf("idx = %d, want 0 (first match)", idx)
	}
}

func TestMatchDeviceIndexNoMatchIsAnError(t *testing.T) {
	names := []string{"Built-in Microphone", "USB Headset"}

	if _, err := matchDeviceIndex(names, "nonexistent"); err == nil {
		t.Fatal("expected a no-match error, got nil")
	}
}

func TestMatchDeviceIndexEmptyNamesList(t *testing.T) {
	if _, err := matchDeviceIndex(nil, "anything"); err == nil {
		t.Fatal("expected an error matching against an empty device list, got nil")
	}
	if _, err := matchDeviceIndex(nil, "0"); err == nil {
		t.Fatal("expected an out-of-range error for index 0 against an empty device list, got nil")
	}
}
