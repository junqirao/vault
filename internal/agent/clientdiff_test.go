package agent

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteClientDiffMultipart 锁定客户端只上传 Needed 文件的协议形状：
// 先 path 字段、再 file 部件，且内容与源文件一致。
func TestWriteClientDiffMultipart(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("hello diff")
	if err := os.WriteFile(filepath.Join(root, "sub", "a.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	files := []clientDiffNeededFile{
		{Path: "sub/a.txt", Kind: "add", Size: int64(len(content))},
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := writeClientDiffMultipart(mw, root, files, nil); err != nil {
		t.Fatalf("写 multipart 失败: %v", err)
	}
	contentType := mw.FormDataContentType()
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" {
		t.Fatalf("content type 非法: %q err=%v", contentType, err)
	}

	mr := multipart.NewReader(&buf, params["boundary"])
	pathPart, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if pathPart.FormName() != "path" {
		t.Fatalf("第一个部件应为 path，实际 %q", pathPart.FormName())
	}
	rawPath, _ := io.ReadAll(pathPart)
	if string(rawPath) != "sub/a.txt" {
		t.Fatalf("path = %q", rawPath)
	}
	filePart, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if filePart.FormName() != "file" {
		t.Fatalf("第二个部件应为 file，实际 %q", filePart.FormName())
	}
	got, _ := io.ReadAll(filePart)
	if !bytes.Equal(got, content) {
		t.Fatalf("文件内容不一致: %q", got)
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Fatalf("应只有两个部件，err=%v", err)
	}
}

func TestLocalDiffPathRejectsEscape(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"../x", "a/../../x", "/etc/passwd", ""} {
		if _, err := localDiffPath(root, rel); err == nil {
			t.Fatalf("路径 %q 应被拒绝", rel)
		}
	}
}
