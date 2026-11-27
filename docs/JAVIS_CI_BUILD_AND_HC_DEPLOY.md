# Hướng dẫn Build bằng GitHub Actions và Cập nhật lên JAVIS HC

Tài liệu này hướng dẫn quy trình build **thủ công khi cần** bằng GitHub Actions từ repository `https://github.com/dinhchinh82/go2rtc` và cách tải artifact về kiểm thử song song / cập nhật lên một bộ JAVIS Home Controller (HC) cụ thể.

> [!IMPORTANT]
> Mọi bản build mới cần được **chạy song song trên port tách biệt (`1985` / `8557` / `8558`)** và kiểm thử thực tế với camera HANET tại hiện trường trước khi thay thế binary đang chạy trên HC. Không restart Home Assistant hay chỉnh logic AI mở cửa khi đang thử nghiệm.

---

## 1. Workflow GitHub Actions hiện có

Repository sử dụng workflow riêng [javis-hanet-build.yml](file:///Volumes/Project/01_Projects/06_VibeCoding/25_go2rtc_patch/go2rtc/.github/workflows/javis-hanet-build.yml) (hoặc `.github/workflows/javis-hanet-build.yml` trên GitHub) chạy **thủ công hoàn toàn (`workflow_dispatch`, không tự chạy khi push/merge)** với 2 job tuần tự:

1. **Job `test` (`Test HANET Compatibility`)** trên `ubuntu-latest`:
   - Cài `ffmpeg` để chạy strict decoder test (`-xerror -err_detect explode`).
   - Kiểm tra `gofmt -l pkg/hanetfix pkg/rtsp internal/rtsp main.go`.
   - Chạy `go vet ./pkg/hanetfix/... ./pkg/rtsp/... ./internal/rtsp/...`.
   - Chạy race tests: `go test -v -race ./pkg/hanetfix/... ./pkg/rtsp/... -timeout 180s`.
   - Chạy E2E RTSP/MP4/WebRTC tests: `go test -v ./internal/rtsp/... -timeout 180s`.
   - Chạy toàn bộ unit tests repo: `go test ./... -timeout 120s`.
   - Upload artifact kết quả test `javis-hanet-test-results` (`hanet-race-results.txt`, `hanet-e2e-results.txt`, `repo-test-results.txt`).
2. **Job `build` (`Build Linux ARM64 (optional AMD64)`, `needs: test`)**:
   - Chỉ chạy khi job `test` pass 100%.
   - Build trực tiếp từ commit hiện tại trong repo (không clone upstream sạch):
     - Mặc định chỉ build `dist/go2rtc_linux_arm64` (`CGO_ENABLED=0 GOOS=linux GOARCH=arm64` — dành cho JAVIS HC).
     - Tùy chọn tick `build_amd64` (mặc định `false`) nếu cần build thêm `dist/go2rtc_linux_amd64`.
   - Tạo `dist/SHA256SUMS.txt` và `dist/build-info.txt` (ghi `commit`, `ref`, `targets`, `go_version`, `app_version`, `test_gate=PASSED`).
   - Upload artifact `go2rtc-javis-hanet-linux`.

---

## 2. Cách kích hoạt Build thủ công trên GitHub Actions

> [!NOTE]
> **Vì sao đôi khi không thấy nút `Run workflow` trên GitHub?**
> GitHub Actions chỉ hiển thị nút **Run workflow** (`workflow_dispatch`) khi file `.github/workflows/javis-hanet-build.yml` đã có mặt trên **nhánh mặc định (default branch, hiện là `master`)**. Nếu workflow mới chỉ nằm trên nhánh `dev`:
> 1. Để hiện nút **Run workflow** trên giao diện Web: merge/cherry-pick `.github/workflows/javis-hanet-build.yml` vào `master` (hoặc đổi default branch của repo sang `dev` trong **Settings -> Branches**).
> 2. Hoặc kích hoạt từ máy local qua GitHub CLI (`gh workflow run javis-hanet-build.yml --ref dev`).

### Cách A: Chạy thủ công trên giao diện Web GitHub
1. Mở repo `https://github.com/dinhchinh82/go2rtc` trên trình duyệt.
2. Vào tab **Actions** -> chọn workflow **JAVIS HANET Build and Test** ở cột trái.
3. Nhấn **Run workflow** -> chọn branch `dev` (hoặc branch cần build); giữ nguyên `build_amd64` tắt nếu chỉ cần `linux/arm64` cho HC -> nhấn **Run workflow**.
4. Chờ cả 2 job **Test HANET Compatibility** và **Build Linux ARM64 (optional AMD64)** chuyển xanh (`Success`).

### Cách B: Chạy và theo dõi qua GitHub CLI (`gh`)
```bash
# Kích hoạt build thủ công mặc định (chỉ ARM64) trên nhánh dev
gh workflow run javis-hanet-build.yml --ref dev

# Hoặc build cả ARM64 + AMD64 khi cần test trên x86_64:
# gh workflow run javis-hanet-build.yml --ref dev -f build_amd64=true

# Xem danh sách các lần chạy gần nhất
gh run list --workflow javis-hanet-build.yml --branch dev --limit 5

# Theo dõi tiến trình run mới nhất theo thời gian thực
gh run watch
```

---

## 3. Tải bản build từ GitHub Actions

### Qua giao diện Web
1. Mở lần chạy **JAVIS HANET Build and Test** đã hoàn tất (xanh) trong tab **Actions**.
2. Kéo xuống mục **Artifacts**.
3. Tải **`go2rtc-javis-hanet-linux`** (sẽ tải về file `.zip`), giải nén ra thư mục tạm trên máy trạm:
   - `go2rtc_linux_arm64` (cho JAVIS HC ARM64: S905X3, RK3588...)
   - `go2rtc_linux_amd64` (cho server/máy test x86_64)
   - `SHA256SUMS.txt`
   - `build-info.txt`

### Qua GitHub CLI (`gh`)
```bash
mkdir -p ./javis-go2rtc-artifact
# Thay <RUN_ID> bằng ID từ lệnh `gh run list`
gh run download <RUN_ID> -n go2rtc-javis-hanet-linux -D ./javis-go2rtc-artifact

cd ./javis-go2rtc-artifact
cat build-info.txt
shasum -a 256 -c SHA256SUMS.txt --ignore-missing   # trên macOS
# hoặc trên Linux: sha256sum -c SHA256SUMS.txt --ignore-missing
```

---

## 4. Copy bản build lên 1 bộ HC cụ thể

Trên máy trạm, copy bộ file cần thiết vào thư mục tạm trên HC (thay `<HC_USER>` và `<HC_IP>` bằng thông tin HC thực tế):

```bash
ssh <HC_USER>@<HC_IP> 'mkdir -p /tmp/go2rtc-hanet-test'
scp go2rtc_linux_arm64 SHA256SUMS.txt build-info.txt <HC_USER>@<HC_IP>:/tmp/go2rtc-hanet-test/
```

SSH vào HC và kiểm tra kiến trúc CPU + SHA256 + version trước khi chạy:

```bash
ssh <HC_USER>@<HC_IP>
cd /tmp/go2rtc-hanet-test

uname -m                      # kỳ vọng: aarch64
sha256sum -c SHA256SUMS.txt --ignore-missing
chmod +x go2rtc_linux_arm64
./go2rtc_linux_arm64 -v
cat build-info.txt
```

---

## 5. Bước 1 trên HC: Chạy thử song song (Khuyến nghị bắt buộc trước khi thay thế)

1. Kiểm tra các port thử nghiệm (`1985`, `8557`, `8558`) đang rảnh và xác định tiến trình `go2rtc` đang chạy hiện tại:
   ```bash
   ss -lntup | grep -E ':(1984|1985|8554|8555|8557|8558)\b' || true
   ps aux | grep -i go2rtc | grep -v grep
   ```
2. Tạo file cấu hình test tách biệt `/tmp/go2rtc-hanet-test/go2rtc-test.yaml` (tham khảo [go2rtc-javis-hanet.example.yaml](../examples/go2rtc-javis-hanet.example.yaml)), thay IP RFC 5737 `192.0.2.x` và `CHANGE_ME` bằng thông tin camera thực tế trên HC:
   ```yaml
   api:
     listen: "127.0.0.1:1985"
   rtsp:
     listen: "127.0.0.1:8557"
   webrtc:
     listen: ":8558"
   streams:
     hanet_good: "rtsp://192.0.2.104:554/user:hanet;pwd:CHANGE_ME"
     hanet_fix_1: "rtsp://192.0.2.105:554/user:hanet;pwd:CHANGE_ME#hanet=1"
   ```
3. Chạy foreground để quan sát log trực tiếp:
   ```bash
   ./go2rtc_linux_arm64 -c /tmp/go2rtc-hanet-test/go2rtc-test.yaml
   ```
4. Ở một terminal SSH khác trên HC, chạy strict decoder từ container Home Assistant (`2024.12.5`):
   ```bash
   URL='rtsp://127.0.0.1:8557/hanet_fix_1'
   docker exec homeassistant timeout -s KILL 25 ffprobe -v error -rtsp_transport tcp -select_streams v:0 -show_entries stream=codec_name,profile,width,height -of json "$URL"
   docker exec homeassistant timeout -s KILL 45 ffmpeg -hide_banner -v error -xerror -err_detect explode -rtsp_transport tcp -threads 1 -i "$URL" -map 0:v:0 -an -frames:v 100 -progress pipe:1 -nostats -f null -
   echo "exit=$?"
   ```
   - Yêu cầu đạt: `codec_name: h264`, `ffmpeg` trả `exit=0`, `frame=100`, không có lỗi stderr.
   - Có thể thêm 1 entity **Generic Camera** thử nghiệm trong HA trỏ vào `rtsp://127.0.0.1:8557/hanet_fix_1` (TCP) để kiểm tra Live/HLS/WebRTC.

---

## 6. Bước 2 trên HC: Cập nhật chính thức lên `go2rtc` đang chạy (sau khi đạt nghiệm thu)

> [!CAUTION]
> Chỉ thực hiện bước này sau khi bản build đã pass strict decode trên các camera lỗi tại HC và đã dừng instance test song song (`Ctrl+C`). Luôn backup binary và config cũ trước khi ghi đè.

1. **Xác định chính xác vị trí binary, file config và service quản lý `go2rtc` trên HC** (tùy đợt triển khai HC có thể dùng systemd service, supervisor hoặc chạy từ `/config/go2rtc`):
   ```bash
   ps aux | grep -i go2rtc | grep -v grep
   systemctl list-units --type=service | grep -i go2rtc || true
   ```
   Ghi lại `<GO2RTC_BIN_PATH>` (ví dụ `/config/go2rtc-1.9.9` hoặc đường dẫn thực tế thấy trong `ps`) và `<GO2RTC_CONFIG_PATH>`.

2. **Backup binary và cấu hình cũ (có timestamp)**:
   ```bash
   TS=$(date +%Y%m%d-%H%M%S)
   sudo cp -a "<GO2RTC_BIN_PATH>" "<GO2RTC_BIN_PATH>.bak.${TS}"
   sudo cp -a "<GO2RTC_CONFIG_PATH>" "<GO2RTC_CONFIG_PATH>.bak.${TS}"
   ls -lh "<GO2RTC_BIN_PATH>"* "<GO2RTC_CONFIG_PATH>"*
   ```

3. **Cập nhật binary mới theo kiểu atomic** (đặt file tạm cùng filesystem rồi `mv`):
   ```bash
   sudo install -m 0755 /tmp/go2rtc-hanet-test/go2rtc_linux_arm64 "<GO2RTC_BIN_PATH>.new"
   "<GO2RTC_BIN_PATH>.new" -v
   sudo mv -f "<GO2RTC_BIN_PATH>.new" "<GO2RTC_BIN_PATH>"
   ```

4. **Bật opt-in `#hanet=1` cho đúng các camera HANET bị lỗi** trong `<GO2RTC_CONFIG_PATH>`:
   - Chỉ thêm `#hanet=1` vào cuối URL của các camera HANET gặp lỗi (giữ nguyên URL của camera bình thường và camera H.265 chuẩn).

5. **Khởi động lại riêng service/tiến trình `go2rtc`** (không cần restart Home Assistant nếu không đổi cấu hình HA):
   ```bash
   # Nếu chạy qua systemd (thay <SERVICE_NAME> theo thực tế):
   sudo systemctl restart <SERVICE_NAME>
   sudo systemctl status <SERVICE_NAME> --no-pager
   ```
   *(Nếu `go2rtc` trên HC chạy trong container riêng hoặc script khởi động riêng, dùng lệnh restart tương ứng của container/script đó.)*

6. **Xác minh sau cập nhật trên port chính (`1984` / `8554`)**:
   ```bash
   "<GO2RTC_BIN_PATH>" -v
   curl -fsS http://127.0.0.1:1984/api/streams | head -c 500
   docker exec homeassistant timeout -s KILL 45 ffmpeg -hide_banner -v error -xerror -err_detect explode -rtsp_transport tcp -threads 1 -i 'rtsp://127.0.0.1:8554/<TEN_STREAM_HANET>' -map 0:v:0 -an -frames:v 100 -progress pipe:1 -nostats -f null -
   ```

---

## 7. Rollback nhanh khi có sự cố

1. Khôi phục binary và config từ bản backup đã tạo:
   ```bash
   sudo cp -a "<GO2RTC_BIN_PATH>.bak.${TS}" "<GO2RTC_BIN_PATH>"
   sudo cp -a "<GO2RTC_CONFIG_PATH>.bak.${TS}" "<GO2RTC_CONFIG_PATH>"
   ```
2. Khởi động lại service `go2rtc`:
   ```bash
   sudo systemctl restart <SERVICE_NAME>
   ```
3. Kiểm tra lại version cũ và stream đối chứng hoạt động bình thường.
