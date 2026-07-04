package connector

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestCafVLQ tests the variable-length quantity encoder used in CAF packet tables.
func TestCafVLQ(t *testing.T) {
	tests := []struct {
		val  int64
		want []byte
	}{
		{0, []byte{0}},
		{1, []byte{1}},
		{127, []byte{0x7F}},
		{128, []byte{0x81, 0x00}},
		{255, []byte{0x81, 0x7F}},
		{16384, []byte{0x81, 0x80, 0x00}},
	}
	for _, tt := range tests {
		got := cafVLQ(tt.val)
		if !bytes.Equal(got, tt.want) {
			t.Errorf("cafVLQ(%d) = %v, want %v", tt.val, got, tt.want)
		}
	}
}

// TestCafVLQ_Negative tests that negative values encode as 0.
func TestCafVLQ_Negative(t *testing.T) {
	got := cafVLQ(-1)
	if !bytes.Equal(got, []byte{0}) {
		t.Errorf("cafVLQ(-1) = %v, want [0]", got)
	}
}

// TestOpusPacketFrames tests Opus TOC byte parsing per RFC 6716.
func TestOpusPacketFrames(t *testing.T) {
	tests := []struct {
		name   string
		packet []byte
		want   int
	}{
		{"empty packet", nil, 960},
		// Config 1 (SILK 20ms), code 0 → 960 samples
		{"SILK 20ms code0", []byte{0x08}, 960},
		// Config 0 (SILK 10ms), code 0 → 480 samples
		{"SILK 10ms code0", []byte{0x00}, 480},
		// Config 2 (SILK 40ms), code 0 → 1920 samples
		{"SILK 40ms code0", []byte{0x10}, 1920},
		// Config 3 (SILK 60ms), code 0 → 2880 samples
		{"SILK 60ms code0", []byte{0x18}, 2880},
		// Config 12 (Hybrid 10ms), code 0 → 480 samples
		{"Hybrid 10ms code0", []byte{0x60}, 480},
		// Config 13 (Hybrid 20ms), code 0 → 960 samples
		{"Hybrid 20ms code0", []byte{0x68}, 960},
		// Config 16 (CELT 2.5ms), code 0 → 120 samples
		{"CELT 2.5ms code0", []byte{0x80}, 120},
		// Config 17 (CELT 5ms), code 0 → 240 samples
		{"CELT 5ms code0", []byte{0x88}, 240},
		// Config 18 (CELT 10ms), code 0 → 480 samples
		{"CELT 10ms code0", []byte{0x90}, 480},
		// Config 19 (CELT 20ms), code 0 → 960 samples
		{"CELT 20ms code0", []byte{0x98}, 960},
		// Code 1: two equal-size frames → double
		{"SILK 20ms code1", []byte{0x09}, 1920},
		// Code 2: two different-size frames → double
		{"SILK 20ms code2", []byte{0x0A}, 1920},
		// Code 3: arbitrary number (5 frames), packet[1] = 5
		{"SILK 20ms code3 x5", []byte{0x0B, 0x05}, 4800},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := opusPacketFrames(tt.packet)
			if got != tt.want {
				t.Errorf("opusPacketFrames = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestOggCRC tests the OGG CRC-32 implementation against known values.
func TestOggCRC(t *testing.T) {
	// Empty data should be 0
	if got := oggCRC(nil); got != 0 {
		t.Errorf("oggCRC(nil) = 0x%08x, want 0", got)
	}

	// Verify CRC is deterministic
	data := []byte("OggS test data")
	crc1 := oggCRC(data)
	crc2 := oggCRC(data)
	if crc1 != crc2 {
		t.Errorf("oggCRC not deterministic: %x != %x", crc1, crc2)
	}

	// Different data should give different CRC
	data2 := []byte("OggS test datb")
	crc3 := oggCRC(data2)
	if crc1 == crc3 {
		t.Error("different data gave same CRC")
	}
}

// TestBuildOpusHead tests the OpusHead packet builder.
func TestBuildOpusHead(t *testing.T) {
	head := buildOpusHead(2, 312)
	if len(head) != 19 {
		t.Fatalf("OpusHead length = %d, want 19", len(head))
	}
	if string(head[0:8]) != "OpusHead" {
		t.Errorf("magic = %q, want %q", string(head[0:8]), "OpusHead")
	}
	if head[8] != 1 {
		t.Errorf("version = %d, want 1", head[8])
	}
	if head[9] != 2 {
		t.Errorf("channels = %d, want 2", head[9])
	}
	preSkip := binary.LittleEndian.Uint16(head[10:12])
	if preSkip != 312 {
		t.Errorf("preSkip = %d, want 312", preSkip)
	}
	sampleRate := binary.LittleEndian.Uint32(head[12:16])
	if sampleRate != 48000 {
		t.Errorf("sampleRate = %d, want 48000", sampleRate)
	}
}

// TestDecodeCAFPacketSizes tests VLQ decoding of CAF packet table entries.
func TestDecodeCAFPacketSizes(t *testing.T) {
	// Encode a few known sizes
	var data []byte
	data = append(data, cafVLQ(100)...)
	data = append(data, cafVLQ(200)...)
	data = append(data, cafVLQ(300)...)

	sizes := decodeCAFPacketSizes(data, 3, false)
	want := []int{100, 200, 300}
	if len(sizes) != len(want) {
		t.Fatalf("got %d sizes, want %d", len(sizes), len(want))
	}
	for i := range want {
		if sizes[i] != want[i] {
			t.Errorf("sizes[%d] = %d, want %d", i, sizes[i], want[i])
		}
	}
}

// TestDecodeCAFPacketSizes_WithFrameSize tests VLQ decoding when frame sizes are present.
func TestDecodeCAFPacketSizes_WithFrameSize(t *testing.T) {
	var data []byte
	// Each entry: byte size VLQ + frame count VLQ
	data = append(data, cafVLQ(100)...)
	data = append(data, cafVLQ(960)...) // frame count (skipped)
	data = append(data, cafVLQ(200)...)
	data = append(data, cafVLQ(960)...)

	sizes := decodeCAFPacketSizes(data, 2, true)
	if len(sizes) != 2 {
		t.Fatalf("got %d sizes, want 2", len(sizes))
	}
	if sizes[0] != 100 || sizes[1] != 200 {
		t.Errorf("sizes = %v, want [100, 200]", sizes)
	}
}

// TestBuildOpusTags tests the OpusTags packet builder.
func TestBuildOpusTags(t *testing.T) {
	tags := buildOpusTags()
	if string(tags[:8]) != "OpusTags" {
		t.Errorf("magic = %q, want %q", string(tags[:8]), "OpusTags")
	}
	vendorLen := binary.LittleEndian.Uint32(tags[8:12])
	vendor := string(tags[12 : 12+vendorLen])
	if vendor != "corten-matrix" {
		t.Errorf("vendor = %q, want %q", vendor, "corten-matrix")
	}
}

// TestConvertAudioForIMessage_NonOGG tests that non-OGG audio is returned unchanged.
func TestConvertAudioForIMessage_NonOGG(t *testing.T) {
	data := []byte("not-opus-data")
	outData, outMime, outName := convertAudioForIMessage(data, "audio/mp4", "test.m4a")
	if !bytes.Equal(outData, data) {
		t.Error("non-OGG data should be returned unchanged")
	}
	if outMime != "audio/mp4" {
		t.Errorf("mime = %q, want %q", outMime, "audio/mp4")
	}
	if outName != "test.m4a" {
		t.Errorf("name = %q, want %q", outName, "test.m4a")
	}
}

// TestConvertAudioForMatrix_NonCAF tests that non-CAF audio is returned unchanged.
func TestConvertAudioForMatrix_NonCAF(t *testing.T) {
	data := []byte("not-caf-data")
	outData, outMime, outName, dur := convertAudioForMatrix(data, "audio/ogg", "test.ogg")
	if !bytes.Equal(outData, data) {
		t.Error("non-CAF data should be returned unchanged")
	}
	if outMime != "audio/ogg" {
		t.Errorf("mime = %q, want %q", outMime, "audio/ogg")
	}
	if outName != "test.ogg" {
		t.Errorf("name = %q, want %q", outName, "test.ogg")
	}
	if dur != 0 {
		t.Errorf("duration = %d, want 0", dur)
	}
}

// TestConvertAudioForIMessage_InvalidOGG tests that invalid OGG data is returned unchanged.
func TestConvertAudioForIMessage_InvalidOGG(t *testing.T) {
	data := []byte("not-real-ogg-data")
	outData, outMime, outName := convertAudioForIMessage(data, "audio/ogg", "test.ogg")
	if !bytes.Equal(outData, data) {
		t.Error("invalid OGG data should be returned unchanged")
	}
	if outMime != "audio/ogg" {
		t.Errorf("mime = %q, want %q", outMime, "audio/ogg")
	}
	if outName != "test.ogg" {
		t.Errorf("name = %q, want %q", outName, "test.ogg")
	}
}

// TestConvertAudioForMatrix_InvalidCAF tests that invalid CAF data is returned unchanged.
func TestConvertAudioForMatrix_InvalidCAF(t *testing.T) {
	data := []byte("not-real-caf-data")
	outData, outMime, outName, dur := convertAudioForMatrix(data, "audio/x-caf", "test.caf")
	if !bytes.Equal(outData, data) {
		t.Error("invalid CAF data should be returned unchanged")
	}
	if outMime != "audio/x-caf" {
		t.Errorf("mime = %q, want %q", outMime, "audio/x-caf")
	}
	if outName != "test.caf" {
		t.Errorf("name = %q, want %q", outName, "test.caf")
	}
	if dur != 0 {
		t.Errorf("duration = %d, want 0", dur)
	}
}

// buildMinimalOGGOpus creates a minimal valid OGG Opus stream for testing.
func buildMinimalOGGOpus() []byte {
	info := &oggOpusInfo{
		Channels:   1,
		PreSkip:    312,
		OpusHead:   buildOpusHead(1, 312),
		GranulePos: 48000, // 1 second
		Packets:    [][]byte{},
	}

	// Create some fake Opus packets (config 19 = CELT 20ms, code 0 = 1 frame = 960 samples)
	for i := 0; i < 50; i++ {
		// TOC byte 0x98 = config 19 (CELT 20ms), code 0
		pkt := make([]byte, 40)
		pkt[0] = 0x98
		info.Packets = append(info.Packets, pkt)
	}

	data, err := writeOGGOpus(info)
	if err != nil {
		panic(err)
	}
	return data
}

// TestOGGtoCAFRoundTrip tests OGG→CAF→OGG conversion preserves audio packets.
func TestOGGtoCAFRoundTrip(t *testing.T) {
	oggData := buildMinimalOGGOpus()

	// Parse OGG
	info1, err := parseOGGOpus(oggData)
	if err != nil {
		t.Fatalf("parseOGGOpus error: %v", err)
	}

	// Convert to CAF
	cafData, err := writeCAFOpus(info1)
	if err != nil {
		t.Fatalf("writeCAFOpus error: %v", err)
	}
	if !bytes.HasPrefix(cafData, []byte("caff")) {
		t.Error("CAF data should start with 'caff'")
	}

	// Parse CAF back
	info2, err := parseCAFOpus(cafData)
	if err != nil {
		t.Fatalf("parseCAFOpus error: %v", err)
	}

	if info2.Channels != info1.Channels {
		t.Errorf("channels: %d != %d", info2.Channels, info1.Channels)
	}
	if len(info2.Packets) != len(info1.Packets) {
		t.Fatalf("packet count: %d != %d", len(info2.Packets), len(info1.Packets))
	}
	for i := range info1.Packets {
		if !bytes.Equal(info1.Packets[i], info2.Packets[i]) {
			t.Errorf("packet %d differs", i)
			break
		}
	}
}

// TestConvertAudioForIMessage_ValidOGG tests full OGG→CAF conversion.
func TestConvertAudioForIMessage_ValidOGG(t *testing.T) {
	oggData := buildMinimalOGGOpus()
	cafData, mime, name := convertAudioForIMessage(oggData, "audio/ogg", "voice.ogg")
	if mime != "audio/x-caf" {
		t.Errorf("mime = %q, want %q", mime, "audio/x-caf")
	}
	if name != "voice.caf" {
		t.Errorf("name = %q, want %q", name, "voice.caf")
	}
	if !bytes.HasPrefix(cafData, []byte("caff")) {
		t.Error("output should be CAF data")
	}
}

// TestConvertAudioForMatrix_ValidCAF tests full CAF→OGG conversion.
func TestConvertAudioForMatrix_ValidCAF(t *testing.T) {
	// First build a valid CAF from OGG
	oggData := buildMinimalOGGOpus()
	info, _ := parseOGGOpus(oggData)
	cafData, _ := writeCAFOpus(info)

	outData, mime, name, dur := convertAudioForMatrix(cafData, "audio/x-caf", "voice.caf")
	if mime != "audio/ogg" {
		t.Errorf("mime = %q, want %q", mime, "audio/ogg")
	}
	if name != "voice.ogg" {
		t.Errorf("name = %q, want %q", name, "voice.ogg")
	}
	if dur <= 0 {
		t.Errorf("duration = %d, want > 0", dur)
	}
	if !bytes.HasPrefix(outData, []byte("OggS")) {
		t.Error("output should be OGG data")
	}
}

// TestParseCAFOpus_TooShort tests that very short input is rejected.
func TestParseCAFOpus_TooShort(t *testing.T) {
	_, err := parseCAFOpus([]byte("caff"))
	if err == nil {
		t.Error("expected error for too-short CAF data")
	}
}

// TestParseCAFOpus_NotCAF tests that non-CAF input is rejected.
func TestParseCAFOpus_NotCAF(t *testing.T) {
	_, err := parseCAFOpus([]byte("not-a-caf-file-at-all"))
	if err == nil {
		t.Error("expected error for non-CAF data")
	}
}

// TestParseOGGOpus_NotOGG tests that non-OGG input is rejected.
func TestParseOGGOpus_NotOGG(t *testing.T) {
	_, err := parseOGGOpus([]byte("not-ogg-data"))
	if err == nil {
		t.Error("expected error for non-OGG data")
	}
}

// TestParseOGGOpus_NotOpus tests that OGG with non-Opus content is rejected.
func TestParseOGGOpus_NotOpus(t *testing.T) {
	// Build a minimal OGG page with non-Opus content
	var buf bytes.Buffer
	buf.WriteString("OggS")
	buf.WriteByte(0) // version
	buf.WriteByte(2) // BOS
	binary.Write(&buf, binary.LittleEndian, int64(0))  // granule
	binary.Write(&buf, binary.LittleEndian, uint32(1))  // serial
	binary.Write(&buf, binary.LittleEndian, uint32(0))  // seq
	binary.Write(&buf, binary.LittleEndian, uint32(0))  // crc
	payload := []byte("VorbisHead\x00\x01\x00\x00\x00\x00\x00\x00\x00")
	buf.WriteByte(byte(1))             // 1 segment
	buf.WriteByte(byte(len(payload)))  // segment size
	buf.Write(payload)

	_, err := parseOGGOpus(buf.Bytes())
	if err == nil {
		t.Error("expected error for non-Opus OGG data")
	}
}

// TestWriteCAFOpus_NoPackets tests that writing with empty packets fails.
func TestWriteCAFOpus_NoPackets(t *testing.T) {
	info := &oggOpusInfo{
		Channels:   1,
		PreSkip:    0,
		OpusHead:   buildOpusHead(1, 0),
		Packets:    nil,
		GranulePos: 0,
	}
	_, err := writeCAFOpus(info)
	if err == nil {
		t.Error("expected error for no packets")
	}
}

// TestConvertAudioForMatrix_FileExtension tests CAF detection by filename.
func TestConvertAudioForMatrix_FileExtension(t *testing.T) {
	// Should attempt CAF parsing based on filename even if mime doesn't match
	data := []byte("not-real-caf")
	_, outMime, _, _ := convertAudioForMatrix(data, "application/octet-stream", "voice.caf")
	// Since the data isn't valid CAF, it should return unchanged
	if outMime != "application/octet-stream" {
		t.Errorf("mime = %q, want %q", outMime, "application/octet-stream")
	}
}
