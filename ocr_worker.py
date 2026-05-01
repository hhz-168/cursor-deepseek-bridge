"""OCR Worker - 透過 stdin/stdout JSON 與 Go bridge 通信。

通訊協議：
  輸入 (stdin): 一行 JSON，格式 {"image": "<base64>"}
  輸出 (stdout): 一行 JSON，格式 {"success": true, "text": "...", "data": [...]}
                 或 {"success": false, "error": "..."}

依賴：pip install rapidocr
"""

import json
import sys
import base64
import traceback
import signal

# 全局標誌用於優雅關閉
_shutdown = False


def _handle_sigterm(signum, frame):
    global _shutdown
    _shutdown = True
    sys.stderr.write("[ocr_worker] received SIGTERM, shutting down...\n")
    sys.stderr.flush()


signal.signal(signal.SIGTERM, _handle_sigterm)


def init_engine():
    """初始化 RapidOCR 引擎（全局單例）"""
    sys.stderr.write("[ocr_worker] initializing RapidOCR engine...\n")
    sys.stderr.flush()

    # 嘗試多種 import 路徑
    RapidOCR = None
    for module_name in ["rapidocr", "rapidocr_onnxruntime"]:
        try:
            m = __import__(module_name, fromlist=["RapidOCR"])
            RapidOCR = m.RapidOCR
            sys.stderr.write(f"[ocr_worker] using OCR module: {module_name}\n")
            sys.stderr.flush()
            break
        except ImportError:
            sys.stderr.write(f"[ocr_worker] {module_name} not available\n")
            sys.stderr.flush()

    if RapidOCR is None:
        raise ImportError(
            "rapidocr not installed. Run: pip install rapidocr"
        )

    sys.stderr.write("[ocr_worker] creating RapidOCR instance...\n")
    sys.stderr.flush()
    engine = RapidOCR()
    sys.stderr.write("[ocr_worker] engine ready\n")
    sys.stderr.flush()
    return engine


def process_image(engine, image_bytes: bytes) -> dict:
    """對圖片 bytes 執行 OCR，返回格式化結果。"""
    # rapidocr 3.x 返回 RapidOCROutput 物件（可直接迭代或用 .to_json()）
    raw_result = engine(image_bytes)

    # RapidOCROutput 物件：直接 to_json() 獲取數據
    if hasattr(raw_result, "to_json"):
        result = raw_result.to_json()
    elif isinstance(raw_result, tuple):
        # 舊版可能返回 (RapidOCROutput, elapse)
        if hasattr(raw_result[0], "to_json"):
            result = raw_result[0].to_json()
        else:
            result = raw_result[0]
    else:
        result = raw_result

    if not result:
        return {
            "success": True,
            "text": "[OCR] (no text detected)",
            "data": [],
        }

    items = []
    text_parts = []

    # result 是 RapidOCROutput.to_json() 返回的 list，每個元素是 dict
    # 格式: {"box": [[x1,y1],[x2,y2],[x3,y3],[x4,y4]], "txt": "文字", "score": 0.999}
    for item in result:
        if isinstance(item, dict):
            box = item.get("box", [])
            txt = item.get("txt", item.get("text", ""))
            score = item.get("score", 0.0)

            # 將坐標轉為 int
            int_box = [[int(x), int(y)] for x, y in box] if box else []
        elif isinstance(item, (list, tuple)):
            # 舊版格式: [[box], (text, score)]
            box_item = item[0]
            text_item = item[1] if len(item) > 1 else ("", 0)
            if isinstance(text_item, (list, tuple)) and len(text_item) >= 2:
                txt, score = text_item[0], text_item[1]
            elif isinstance(text_item, str):
                txt, score = text_item, 0.0
            else:
                continue
            int_box = [[int(x), int(y)] for x, y in box_item] if isinstance(box_item, list) else []
        else:
            continue

        entry = {
            "text": str(txt),
            "box": int_box,
            "score": round(float(score), 4),
        }
        items.append(entry)

        # 可讀格式
        coords = "-".join(f"({x},{y})" for x, y in int_box) if int_box else "N/A"
        text_parts.append(f"  text={json.dumps(str(txt))} box=[{coords}] score={score:.4f}")

    # 構建返回文字
    lines = ["[OCR Result]"]
    lines.extend(text_parts)
    lines.append("[OCR Raw JSON]")
    lines.append(json.dumps(items, ensure_ascii=False))

    return {
        "success": True,
        "text": "\n".join(lines),
        "data": items,
    }


def send_response(response: dict):
    """發送 JSON 響應到 stdout"""
    json_str = json.dumps(response, ensure_ascii=False) + "\n"
    sys.stdout.buffer.write(json_str.encode("utf-8"))
    sys.stdout.buffer.flush()


def main():
    try:
        engine = init_engine()
    except Exception as e:
        resp = {
            "success": False,
            "error": f"Failed to initialize OCR engine: {e}",
            "traceback": traceback.format_exc(),
        }
        sys.stderr.write(f"[ocr_worker] FATAL: {e}\n")
        sys.stderr.flush()
        send_response(resp)
        return

    # 發送就緒信號
    sys.stderr.write("[ocr_worker] ready, waiting for requests...\n")
    sys.stderr.flush()

    while not _shutdown:
        try:
            line = sys.stdin.buffer.readline().decode("utf-8").strip()
        except (IOError, EOFError, KeyboardInterrupt):
            sys.stderr.write("[ocr_worker] stdin closed, exiting\n")
            sys.stderr.flush()
            break

        if not line:
            if _shutdown:
                break
            continue

        try:
            request = json.loads(line)
            image_b64 = request.get("image", "")
            if not image_b64:
                response = {"success": False, "error": "missing image field"}
            else:
                # 解碼 base64
                image_data = base64.b64decode(image_b64)
                response = process_image(engine, image_data)
        except Exception as e:
            response = {
                "success": False,
                "error": str(e),
                "traceback": traceback.format_exc(),
            }
            sys.stderr.write(f"[ocr_worker] error processing request: {e}\n")
            sys.stderr.flush()

        # 輸出結果（一行 JSON）
        try:
            send_response(response)
        except (IOError, BrokenPipeError):
            sys.stderr.write("[ocr_worker] stdout broken, exiting\n")
            sys.stderr.flush()
            break


if __name__ == "__main__":
    main()
