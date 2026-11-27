# JAVIS HANET RTSP Compatibility Fix

## 1. Tổng quan

Module `pkg/hanetfix` khắc phục lỗi camera AI HANET phát bitstream **H.264** nhưng RTSP server khai báo `H265/90000` (không có `a=fmtp`) trên SDP `s=rtsp_demo` và đóng gói các NAL slice phân mảnh bằng header HEVC FU giả (`0x63 <byte2_nal> <flags|fuType>` với `fuType` = `48` hoặc `50`).

- **Không transcode video**, không phụ thuộc FFmpeg khi chạy thực tế.
- **Opt-in bắt buộc** qua fragment `#hanet=1` trên source URL (`rtsp://...#hanet=1`).
- Tự động phân biệt camera lỗi với **H.264 chuẩn** và **HEVC (H.265) chuẩn** (kể cả HEVC không có `fmtp` nhưng có parameter set in-band).
- Giữ nguyên track audio (như `MPEG4-GENERIC/48000/1` AAC-LC) và tương thích hoàn toàn với các output native của `go2rtc` (RTSP Server, MP4/MSE, WebRTC).

## 2. Cách sử dụng

Thêm `#hanet=1` vào cuối URL RTSP của camera cần xử lý (hỗ trợ kết hợp cùng các option khác của `go2rtc` như `#timeout=5#hanet=1`):

```yaml
streams:
  hanet_good: "rtsp://192.0.2.104:554/user:hanet;pwd:CHANGE_ME"
  hanet_fix: "rtsp://192.0.2.105:554/user:hanet;pwd:CHANGE_ME#hanet=1"
```

## 3. Kiến trúc và cơ chế an toàn

1. **Tầng URL handler ([internal/rtsp/rtsp.go](../internal/rtsp/rtsp.go))**:
   - Đọc `#hanet=1`, tắt backchannel trên kết nối opt-in và từ chối sớm giao thức/transport không hỗ trợ (`rtsps://`, `#transport=udp`, ...).
   - Gắn listener log có kiểm soát tần suất (rate-limited) và che mật khẩu (`pwd:****`).
2. **Tầng RTSP Client ([pkg/rtsp/hanet_javis.go](../pkg/rtsp/hanet_javis.go), [client.go](../pkg/rtsp/client.go), [conn.go](../pkg/rtsp/conn.go))**:
   - Sau `DESCRIBE`, nếu SDP khớp prefilter (`s=rtsp_demo` + duy nhất 1 video track `H265/90000` không có `a=fmtp:<pt>`), client thực hiện probe ngắn trên phiên hiện tại (giới hạn thời gian mặc định 8s, tối đa 4000 packet / 8MB).
   - Nếu `Classifier` xác nhận **HEVC thật**: đóng probe, mở lại phiên sạch và giữ nguyên `H265` (passthrough).
   - Nếu `Classifier` xác nhận **HANET lỗi** (có H.264 SPS + PPS hợp lệ khớp ID + ít nhất 1 chuỗi IDR FU hoàn chỉnh hợp lệ và không có NAL HEVC thật): đóng probe, mở phiên mới, xác minh lại SDP không đổi rồi sửa duy nhất video track thành `H264/90000` kèm `a=fmtp` lấy từ SPS/PPS thực tế của chính camera.
   - Nếu luồng **mơ hồ** hoặc timeout: **fail-closed**, không ép sang H.264.
3. **Tầng RTP Repair ([pkg/hanetfix/repair.go](../pkg/hanetfix/repair.go))**:
   - Trước khi đẩy packet vào `core.Receiver`, `Converter.Process` phục hồi header H.264 FU-A (chỉ chèn byte `p[1]` vào gói Start), buffer các fragment theo NAL hoàn chỉnh để loại bỏ frame vỡ khi mất gói, giữ nguyên timestamp 90kHz / Marker / SSRC, đảm bảo sequence number đầu ra đơn điệu liên tục kể cả khi wrap `uint16`, và bắt đầu luồng tại biên SPS + PPS + IDR hoàn chỉnh.
   - Nếu camera đổi sang HEVC thật khi đang stream (`ErrCodecMismatch`) hoặc đổi codec khi reconnect, kết nối dừng rõ ràng để stream reconnect/renegotiate.

## 4. Giới hạn

- Chỉ hỗ trợ `rtsp://` với `RTP/AVP/TCP` interleaved (không áp dụng cho UDP, `rtsps://` hay backchannel).
- Chỉ nhắm vào signature HANET đã xác minh (`s=rtsp_demo`, không `fmtp`, `0x63` với `FuType` 48/50). Không tự sửa các lỗi HEVC khác.
- Chạy ổn định trên mock/fixture; hiệu năng và độ ổn định dài hạn trên phần cứng HC ARM64 thực tế (24–72h soak) cần nghiệm thu trực tiếp tại công trình.

## 5. Triển khai thử nghiệm song song trên JAVIS HC

Xem hướng dẫn chi tiết cách build bằng GitHub Actions, tải artifact và cập nhật lên một bộ HC cụ thể tại [JAVIS_CI_BUILD_AND_HC_DEPLOY.md](JAVIS_CI_BUILD_AND_HC_DEPLOY.md).

Không thay thế binary đang chạy (`/config/go2rtc-1.9.9`) và không restart Home Assistant. Sử dụng cấu hình mẫu tại [examples/go2rtc-javis-hanet.example.yaml](../examples/go2rtc-javis-hanet.example.yaml) với các port tách biệt:

- API: `127.0.0.1:1985`
- RTSP: `127.0.0.1:8557`
- WebRTC: `:8558`

Kiểm tra port rảnh và chạy foreground trên HC:

```bash
ss -lntup | grep -E ':(1985|8557|8558)\b' || true
sha256sum -c SHA256SUMS.txt --ignore-missing
./go2rtc_linux_arm64 -v
./go2rtc_linux_arm64 -c ./go2rtc-javis.local.yaml
```

Kiểm thử bằng FFprobe/FFmpeg sẵn có trong container Home Assistant (2024.12.5, FFmpeg 6.1.1):

```bash
URL='rtsp://127.0.0.1:8557/hanet_fix_1'
docker exec homeassistant timeout -s KILL 25 ffprobe -v error -rtsp_transport tcp -select_streams v:0 -show_entries stream=codec_name,profile,width,height -of json "$URL"
docker exec homeassistant timeout -s KILL 45 ffmpeg -hide_banner -v error -xerror -err_detect explode -rtsp_transport tcp -threads 1 -i "$URL" -map 0:v:0 -an -frames:v 100 -progress pipe:1 -nostats -f null -
echo "exit=$?"
```

Sau khi FFmpeg strict decode đạt 100+ frame (`exit=0`, không lỗi stderr), thêm **Generic Camera** thử nghiệm mới trong HA trỏ tới `rtsp://127.0.0.1:8557/hanet_fix_1` (TCP) để kiểm tra snapshot, HLS và WebRTC.

## 6. Rollback

1. Dừng tiến trình `go2rtc_linux_arm64` thử nghiệm (`Ctrl+C` hoặc `kill`).
2. Xóa entity Generic Camera thử nghiệm (hoặc trả URL camera về cấu hình trước đó nếu có chỉnh).
3. File cấu hình local và binary thử nghiệm tách biệt hoàn toàn nên `go2rtc` sản xuất trên port `1984/8554/8555` và HA không bị ảnh hưởng.
