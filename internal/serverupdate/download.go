package serverupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	downloadAttempts   = 3
	downloadPartSuffix = ".part"
)

type progressFunc func(received, total int64)

// downloadAsset 下载一个更新包到 partPath，支持断点续传与边下边算 sha256。
//
// resolveURL 在资产地址过期（403/404）时重新查询一次；expectedSHA 为空时只校验大小，
// 调用方应尽量保证有摘要（GitHub asset digest 或 SHA256SUMS.txt）。
func downloadAsset(
	ctx context.Context,
	client *http.Client,
	assetURL string,
	resolveURL func(context.Context) (string, error),
	partPath string,
	expectedSize int64,
	expectedSHA string,
	prog progressFunc,
) error {
	if client == nil {
		client = http.DefaultClient
	}
	var lastErr error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(1<<(attempt-2)) * time.Second):
			}
		}
		url := assetURL
		err := downloadOnce(ctx, client, url, resolveURL, partPath, expectedSize, expectedSHA, prog)
		if err == nil {
			return nil
		}
		lastErr = err
		// 地址过期/权限变化：重新查询资产地址后再试。
		if isExpiredAssetError(err) && resolveURL != nil {
			next, rerr := resolveURL(ctx)
			if rerr == nil && strings.TrimSpace(next) != "" {
				assetURL = next
				continue
			}
		}
	}
	return lastErr
}

type httpStatusError struct {
	status int
	body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.status, strings.TrimSpace(e.body))
}

func isExpiredAssetError(err error) bool {
	se, ok := err.(*httpStatusError)
	if !ok {
		return false
	}
	switch se.status {
	case http.StatusForbidden, http.StatusNotFound, http.StatusGone, http.StatusUnauthorized:
		return true
	}
	return false
}

func downloadOnce(
	ctx context.Context,
	client *http.Client,
	assetURL string,
	resolveURL func(context.Context) (string, error),
	partPath string,
	expectedSize int64,
	expectedSHA string,
	prog progressFunc,
) error {
	offset, digest, err := resumeState(partPath)
	if err != nil {
		return fmt.Errorf("读取续传状态失败: %w", err)
	}
	// 已下载文件比目标还大：说明缓存损坏，直接丢弃重来。
	if expectedSize > 0 && offset > expectedSize {
		_ = os.Remove(partPath)
		offset, digest = 0, sha256.New()
	}
	// 上次已经下完但还没改名：直接校验，避免 Range 416。
	if expectedSize > 0 && offset == expectedSize {
		if expectedSHA == "" || strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), expectedSHA) {
			if prog != nil {
				prog(offset, offset)
			}
			return nil
		}
		_ = os.Remove(partPath)
		offset, digest = 0, sha256.New()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", githubUserAgent)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		if offset > 0 {
			// 服务端不支持 Range：丢掉前缀从头下载。
			offset, digest = 0, sha256.New()
			if err := os.Remove(partPath); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	case http.StatusPartialContent:
		// 正常续传。
	case http.StatusRequestedRangeNotSatisfiable:
		// 本地 .part 与服务端不一致：丢掉后下一次从头下载。
		_ = os.Remove(partPath)
		return &httpStatusError{status: resp.StatusCode, body: "range not satisfiable"}
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		return &httpStatusError{status: resp.StatusCode, body: string(body)}
	}

	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	if offset == 0 {
		if err := f.Truncate(0); err != nil {
			return err
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}

	total := expectedSize
	if total <= 0 && resp.ContentLength >= 0 {
		if resp.StatusCode == http.StatusPartialContent {
			total = offset + resp.ContentLength
		} else {
			total = resp.ContentLength
		}
	}
	if prog != nil {
		prog(offset, total)
	}

	var src io.Reader = resp.Body
	if total > 0 {
		src = io.LimitReader(resp.Body, total-offset+1)
	}
	written, err := io.Copy(&progressWriter{w: io.MultiWriter(f, digest), prog: prog, total: total, received: offset}, src)
	if err != nil {
		// 中断的响应体保留 .part，下次续传。
		return err
	}
	gotSize := offset + written
	if total > 0 && gotSize != total {
		_ = os.Remove(partPath)
		return fmt.Errorf("下载大小不符：期望 %d，实际 %d", total, gotSize)
	}
	gotSHA := hex.EncodeToString(digest.Sum(nil))
	if expectedSHA != "" && !strings.EqualFold(gotSHA, expectedSHA) {
		_ = os.Remove(partPath)
		return fmt.Errorf("sha256 不符：期望 %s，实际 %s", expectedSHA, gotSHA)
	}
	if prog != nil {
		prog(gotSize, gotSize)
	}
	return nil
}

type progressWriter struct {
	w        io.Writer
	prog     progressFunc
	total    int64
	received int64
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if n > 0 && p.prog != nil {
		p.received += int64(n)
		p.prog(p.received, p.total)
	}
	return n, err
}

// resumeState 返回 part 文件已有字节数与哈希状态。
func resumeState(partPath string) (int64, hash.Hash, error) {
	digest := sha256.New()
	f, err := os.Open(partPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, digest, nil
		}
		return 0, nil, err
	}
	defer func() { _ = f.Close() }()
	n, err := io.Copy(digest, f)
	if err != nil {
		return 0, nil, err
	}
	return n, digest, nil
}
