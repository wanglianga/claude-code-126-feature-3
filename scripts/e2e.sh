#!/usr/bin/env bash
# 端到端冒烟测试（128 项断言）：覆盖接单→库存→出杯→封签分袋→骑手到店→核验交接→异常→财务→报表，
# 以及放置过久重做的并发交接（规则引擎/安全窗口/wait_new/take_original/abort/旧码失效/责任复核/补偿兑现）全链路
set -uo pipefail
BASE="${BASE:-http://localhost:8099}"
CURL="curl -s --noproxy *"
PASS=0; FAIL=0
ok(){ echo "PASS: $1"; PASS=$((PASS+1)); }
bad(){ echo "FAIL: $1"; FAIL=$((FAIL+1)); }

# api <method> <path> <staff_code> [json] [idempotency-key]
api(){
  local m="$1" p="$2" code="$3" body="${4:-}" idem="${5:-}"
  if [ -n "$body" ]; then
    if [ -n "$idem" ]; then
      curl -s --noproxy '*' -X "$m" "$BASE$p" -H "X-Staff-Code: $code" \
        -H "Content-Type: application/json" -H "Idempotency-Key: $idem" -d "$body"
    else
      curl -s --noproxy '*' -X "$m" "$BASE$p" -H "X-Staff-Code: $code" \
        -H "Content-Type: application/json" -d "$body"
    fi
  else
    curl -s --noproxy '*' -X "$m" "$BASE$p" -H "X-Staff-Code: $code"
  fi
}
# http_code: 单次请求，响应体存 /tmp/cc126/last.json，HTTP 码打印到 stdout（调用方用 RC=$(...) 捕获）
api_code(){
  local m="$1" p="$2" code="$3" body="${4:-}" idem="${5:-}"
  local args=(-s --noproxy '*' -o /tmp/cc126/last.json -w '%{http_code}' -X "$m" "$BASE$p" \
    -H "X-Staff-Code: $code" -H "Content-Type: application/json")
  [ -n "$idem" ] && args+=(-H "Idempotency-Key: $idem")
  [ -n "$body" ] && args+=(-d "$body")
  curl "${args[@]}"
}
J(){ jq -r "$1" /tmp/cc126/last.json; }
expect_code(){ # expected desc ; use last call's code var $RC
  if [ "$RC" = "$1" ]; then ok "$2 (HTTP $RC)"; else bad "$2 (期望 $1 实际 $RC): $(cat /tmp/cc126/last.json)"; fi
}
save(){ cp /tmp/cc126/last.json "/tmp/cc126/saved_$1.json"; }
getf(){ jq -r "$1" "/tmp/cc126/saved_$2.json"; }

PSQL="psql -h /tmp/cc126 -p 5433 -U coffee -d coffee -tA"

# 重置数据库与 Redis（测试脚本专用：drop/create 后由服务启动迁移自动重建）
if [ "${SKIP_RESET:-0}" != "1" ]; then
  echo "== 重置测试环境 =="
  pgrep -x server | xargs -r kill
  sleep 1.5
  psql -h /tmp/cc126 -p 5433 -U coffee -d postgres \
    -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='coffee' AND pid<>pg_backend_pid();" >/dev/null
  psql -h /tmp/cc126 -p 5433 -U coffee -d postgres -c "DROP DATABASE IF EXISTS coffee;" -c "CREATE DATABASE coffee;" >/dev/null
  redis-cli -p 6380 FLUSHALL >/dev/null
  DATABASE_URL="postgres://coffee@127.0.0.1:5433/coffee?sslmode=disable" \
    REDIS_ADDR=localhost:6380 PORT=8099 nohup /tmp/cc126/server > /tmp/cc126/app.log 2>&1 &
  for i in $(seq 1 20); do
    curl -s --noproxy '*' "$BASE/healthz" | grep -q '"status":"ok"' && break
    sleep 0.5
  done
fi

echo "== 0. 健康检查 =="
curl -s --noproxy '*' "$BASE/healthz" | jq -e '.postgres=="ok" and .redis=="ok"' >/dev/null && ok "healthz" || bad "healthz"

echo "== 1. 接单：冰拿铁+热美式+冰套餐+轻食 =="
BODY='{"platform_order_no":"EM-E2E-1","store_id":1,"customer_note":"杯盖贴紧不要摇","rider_eta":"2030-01-01T00:00:00Z",
 "items":[{"sku":"LT","qty":1,"temp":"ice","sugar":"三分糖","cup_size":"L","note":"少冰"},
          {"sku":"AM","qty":1,"temp":"hot","sugar":"无糖","cup_size":"M"},
          {"sku":"SET","qty":1,"temp":"ice","sugar":"五分糖","cup_size":"M"},
          {"sku":"LF","qty":1,"temp":"none","sugar":"无糖","cup_size":"-"}]}'
RC=$(api_code POST /v1/platform/orders 9001 "$BODY"); expect_code 201 "接单成功"
save order1
OID=$(getf .id order1)
[ "$(J '.cups|length')" = "4" ] && ok "生成 4 杯" || bad "杯数: $(cat /tmp/cc126/last.json)"

echo "== 2. 库存扣减校验 =="
api GET /v1/stores/1/inventory 0000 > /tmp/cc126/last.json
MILK=$(J '.[]|select(.sku=="milk")|.qty'); SEAL=$(J '.[]|select(.sku=="seal")|.qty')
STRAW=$(J '.[]|select(.sku=="straw")|.qty'); UT=$(J '.[]|select(.sku=="utensil")|.qty')
[ "$MILK" = "19820" ] && ok "牛奶扣 180ml (20000->$MILK)" || bad "牛奶库存 $MILK"
[ "$SEAL" = "797" ] && ok "3 杯饮品各扣 1 张封签，轻食不贴 (800->$SEAL)" || bad "封签 $SEAL"
[ "$STRAW" = "398" ] && ok "冰饮 2 杯扣 2 支吸管 (400->$STRAW)" || bad "吸管 $STRAW"
[ "$UT" = "198" ] && ok "轻食/套餐扣 2 份餐具 (200->$UT)" || bad "餐具 $UT"

echo "== 3. 幂等重推 =="
BODYIDEM='{"platform_order_no":"EM-E2E-IDEM","store_id":1,"items":[{"sku":"AM","qty":1,"temp":"hot","sugar":"无糖","cup_size":"M"}]}'
api POST /v1/platform/orders 9001 "$BODYIDEM" "key-e2e-1" > /tmp/cc126/r1.json
api POST /v1/platform/orders 9001 "$BODYIDEM" "key-e2e-1" > /tmp/cc126/r2.json
[ "$(jq -r .error /tmp/cc126/r1.json)" = "null" ] && diff <(jq -S . /tmp/cc126/r1.json) <(jq -S . /tmp/cc126/r2.json) >/dev/null \
  && ok "幂等返回一致（同 Idempotency-Key 不重复建单）" || bad "幂等失效: $(cat /tmp/cc126/r1.json)"
IDEM1=$(jq -r .id /tmp/cc126/r1.json); IDEM2=$(jq -r .id /tmp/cc126/r2.json)
[ "$IDEM1" = "$IDEM2" ] && ok "两次返回同一订单 id=$IDEM1" || bad "订单 id 不一致 $IDEM1/$IDEM2"
# 做完幂等单，避免污染后续出杯队列
ICID=$(api GET "/v1/orders/$IDEM1" 0000 | jq -r '.cups[0].id')
api POST /v1/staff/stores/1/cups/next 3001 '{"store_id":1}' >/dev/null
api POST "/v1/staff/cups/$ICID/seal" 3001 >/dev/null
api POST /v1/rider/handoffs 4001 "{\"order_id\":$IDEM1,\"actual_bags\":1,\"actual_hot\":1}" >/dev/null

echo "== 4. 出杯队列顺序 + 顾客备注可见 =="
api GET /v1/staff/stores/1/queue 3001 > /tmp/cc126/last.json
[ "$(J 'length')" = "4" ] && ok "队列 4 杯" || bad "队列长度 $(J 'length')"
[ "$(J '.[0].customer_note')" = "杯盖贴紧不要摇" ] && ok "队列显示顾客备注" || bad "顾客备注缺失"

echo "== 5. 逐杯制作+封签，冷热分袋 =="
for i in 1 2 3 4; do
  api POST /v1/staff/stores/1/cups/next 3001 '{"store_id":1}' > /tmp/cc126/next.json
  CID=$(jq -r .id /tmp/cc126/next.json)
  RC=$(api_code POST "/v1/staff/cups/$CID/seal" 3001)
  [ "$RC" = "200" ] || { bad "第 $i 杯封签失败: $(cat /tmp/cc126/last.json)"; }
done
ok "4 杯依次叫号制作并封签"
api GET "/v1/orders/$OID" 0000 > /tmp/cc126/last.json
[ "$(J .status)" = "active" ] && ok "全部封齐进入待取区(active)" || bad "订单状态 $(J .status)"
NB=$(J '.bags|map(.bag_code)|unique|length')
[ "$NB" = "3" ] && ok "冷热餐食 3 个物理袋 ($NB)" || bad "物理袋数 $NB"
api GET "/v1/staff/orders/$OID/bag-labels" 3001 > /tmp/cc126/last.json
[ "$(J '.labels|length')" = "3" ] && ok "袋贴 3 张" || bad "袋贴数"
api GET "/v1/staff/orders/$OID/cup-labels" 3001 > /tmp/cc126/last.json
[ "$(J '[.labels[].seal_code]|map(length>0)|all')" = "true" ] && ok "杯贴含封签码/冰热糖度杯型" || bad "杯贴数据"

echo "== 6. 骑手提前到店 =="
BODY2='{"platform_order_no":"EM-E2E-2","store_id":1,"rider_eta":"2030-01-01T00:00:00Z",
 "items":[{"sku":"AM","qty":1,"temp":"hot","sugar":"全糖","cup_size":"M"}]}'
RC=$(api_code POST /v1/platform/orders 9001 "$BODY2"); expect_code 201 "第二单接单"
save order2; OID2=$(getf .id order2)
RC=$(api_code POST /v1/rider/arrivals 4001 "{\"order_id\":$OID2}"); expect_code 200 "骑手到店登记"
[ "$(J '.orders[0].early')" = "true" ] && ok "未出杯判定为提前到店" || bad "early 标志"
[ "$(J '.orders[0].exception_id')" != "null" ] && ok "自动生成 rider_early 异常" || bad "无早到异常"
api GET "/v1/rider/screen?store_id=1" 4001 > /tmp/cc126/last.json
[ "$(J "map(select(.order_id==$OID2))|length")" = "1" ] && ok "骑手到店屏显示等待单" || bad "到店屏"

echo "== 7. 未出杯禁止交接 =="
RC=$(api_code POST /v1/rider/handoffs 4001 "{\"order_id\":$OID2,\"actual_bags\":1}")
expect_code 409 "未出杯交接被拒"
[ "$(J .error)" = "drinks_not_ready" ] || bad "错误类型 $(J .error)"

echo "== 8. 出杯后核验：先模拟取错袋，再正确交接 =="
CID2=$(api GET "/v1/orders/$OID2" 0000 | jq -r '.cups[0].id')
api POST /v1/staff/stores/1/cups/next 3001 '{"store_id":1}' > /dev/null
api POST "/v1/staff/cups/$CID2/seal" 3001 > /dev/null
RC=$(api_code POST /v1/rider/handoffs 4001 "{\"order_id\":$OID2,\"actual_bags\":2}")
expect_code 409 "袋数不符核验不通过"
[ "$(J '.issues|index("wrong_bag_count")')" != "null" ] && ok "识别取错袋问题" || bad "issues: $(cat /tmp/cc126/last.json)"
RC=$(api_code POST /v1/rider/handoffs 4001 "{\"order_id\":$OID2,\"actual_bags\":1,\"actual_hot\":1,\"actual_cold\":0}")
expect_code 200 "核验通过完成交接"
[ "$(J .status)" = "handed" ] && ok "订单状态 handed" || bad "状态 $(J .status)"

echo "== 9. 订单1：漏餐具核验失败后补放交接 =="
RC=$(api_code POST /v1/rider/handoffs 4001 "{\"order_id\":$OID,\"actual_bags\":3,\"actual_hot\":1,\"actual_cold\":1,\"actual_food\":1,\"has_straw\":false,\"has_utensil\":false}")
expect_code 409 "漏吸管餐具核验不通过"
[ "$(J '.issues|index("missing_straw")')" != "null" ] && [ "$(J '.issues|index("missing_utensil")')" != "null" ] \
  && ok "同时识别漏吸管+漏餐具" || bad "issues: $(cat /tmp/cc126/last.json)"
RC=$(api_code POST /v1/rider/handoffs 4001 "{\"order_id\":$OID,\"actual_bags\":3,\"actual_hot\":1,\"actual_cold\":1,\"actual_food\":1,\"has_straw\":true,\"has_utensil\":true}")
expect_code 200 "补齐后交接成功"

echo "== 10. 顾客漏液投诉 + 财务定责 =="
RC=$(api_code POST "/v1/customers/orders/$OID/complain" 7001 '{"type":"leak_complaint","detail":"拿铁漏液半杯"}')
expect_code 201 "投诉受理"
EXC=$(J .exception_id)
RC=$(api_code PATCH "/v1/finance/exceptions/$EXC" 6001 '{"responsibility":"shared","comp_amount":22,"platform_share":10,"resolution":"门店封盖不当与平台共担"}')
expect_code 200 "财务定责完成"
api GET "/v1/stores/1/finance?from=2026-09-01&to=2030-12-31" 6001 > /tmp/cc126/last.json
[ "$(J '[.summary[]|select(.kind=="customer_comp")]|length')" -ge 1 ] && ok "补偿入财务台账" || bad "财务无补偿记录"
[ "$(J '[.summary[]|select(.kind=="platform_comp")]|length')" -ge 1 ] && ok "平台分摊到账" || bad "无平台分摊"
[ "$(J '[.summary[]|select(.kind=="revenue")]|length')" -ge 1 ] && ok "交接订单确认营业收入" || bad "无收入记录"

echo "== 11. 重做（放置过久）：旧杯报损、新杯回队、成本入班组 =="
BODY3='{"platform_order_no":"EM-E2E-3","store_id":1,"items":[{"sku":"CB","qty":1,"temp":"ice","sugar":"无糖","cup_size":"L"}]}'
RC=$(api_code POST /v1/platform/orders 9001 "$BODY3"); expect_code 201 "第三单接单"
save order3; OID3=$(getf .id order3)
api POST /v1/staff/stores/1/cups/next 3001 '{"store_id":1}' > /tmp/cc126/n3.json
CID3=$(jq -r .id /tmp/cc126/n3.json)
api POST "/v1/staff/cups/$CID3/seal" 3001 > /dev/null
RC=$(api_code POST "/v1/staff/cups/$CID3/remake" 3001 '{"reason":"stale_drink","detail":{"waited_minutes":20}}')
expect_code 201 "过久重做受理"
[ "$(J .loss_cost)" != "0" ] && ok "重做产生报损成本 ($(J .loss_cost))" || bad "重做成本为 0"
api GET "/v1/orders/$OID3" 0000 > /tmp/cc126/last.json
[ "$(J '.cups|map(select(.status=="queued"))|length')" = "1" ] && ok "新杯重新入队" || bad "新杯未入队"
NEWCID=$(J '.cups|map(select(.status=="queued"))[0].id')
api POST "/v1/staff/cups/$NEWCID/start" 3001 > /dev/null
NEWSEAL=$(api POST "/v1/staff/cups/$NEWCID/seal" 3001 | jq -r '.cup.seal_code')
api GET "/v1/staff/stores/1/loss-reports" 3001 > /tmp/cc126/last.json
[ "$(J "[.[]|select(.reason==\"stale_drink\")]|length")" -ge 1 ] && ok "报损记录可查" || bad "无报损记录"
# 旧封签码立即失效：不扫新码 / 扫旧码均被拒
api GET "/v1/orders/$OID3" 0000 > /tmp/cc126/last.json
OLDSEAL3=$(J '[.cups[]|select(.status=="loss_hold")][0].seal_code')
RC=$(api_code POST /v1/rider/handoffs 4001 "{\"order_id\":$OID3,\"actual_bags\":1,\"actual_cold\":1,\"has_straw\":true}")
[ "$RC" = "409" ] && [ "$(J .error)" = "must_scan_new_seal" ] && ok "重做后必须扫新封签码" || bad "新码强校验: $RC $(cat /tmp/cc126/last.json)"
RC=$(api_code POST /v1/rider/handoffs 4001 "{\"order_id\":$OID3,\"actual_bags\":1,\"actual_cold\":1,\"has_straw\":true,\"seal_codes\":[\"$OLDSEAL3\"]}")
[ "$RC" = "409" ] && [ "$(J .error)" = "old_seal_invalid" ] && ok "扫旧封签码被拒（旧码立即失效）" || bad "旧码失效: $RC $(cat /tmp/cc126/last.json)"
RC=$(api_code POST /v1/rider/handoffs 4001 "{\"order_id\":$OID3,\"actual_bags\":1,\"actual_cold\":1,\"has_straw\":true,\"seal_codes\":[\"$NEWSEAL\"]}")
expect_code 200 "扫新封签码完成交接"

echo "== 12. 缺货自动临时下架（冷萃液 260ml，单杯需 300ml） =="
$PSQL -c "UPDATE inventory SET qty=260 WHERE ingredient_id=(SELECT id FROM ingredients WHERE sku='coldbase') AND store_id=1" >/dev/null
BODY4='{"platform_order_no":"EM-E2E-4","store_id":1,"items":[{"sku":"CB","qty":1,"temp":"ice","sugar":"无糖","cup_size":"L"}]}'
RC=$(api_code POST /v1/platform/orders 9001 "$BODY4"); expect_code 409 "库存不足接单被拒"
[ "$(J .error)" = "stock_shortage" ] && ok "错误码 stock_shortage" || bad "错误码 $(J .error)"
[ "$(J .details.short_ingredient)" = "coldbase" ] && ok "定位缺货原料 coldbase" || bad "缺货原料 $(J .details)"
RC=$(api_code POST /v1/platform/orders 9001 "$BODY4"); expect_code 409 "缺货商品再次接单被拒"
[ "$(J .error)" = "item_delisted" ] && ok "冷萃已自动临时下架" || bad "未下架 $(J .error)"
api GET /v1/stores/1/delistings 3001 > /tmp/cc126/last.json
[ "$(J '[.[]|select(.sku=="CB" and .active==true)]|length')" = "1" ] && ok "下架记录可查" || bad "无下架记录"

echo "== 13. 补货后自动恢复上架 =="
RC=$(api_code POST /v1/staff/inventory/restock 3001 '{"store_id":1,"resolve_delistings":true,"items":[{"ingredient_sku":"coldbase","qty":5000}]}')
expect_code 200 "冷萃液补货"
[ "$(J '.relisted_menu_item_ids|index(3)')" != "null" ] && ok "原料齐备自动恢复 CB 上架" || bad "未自动上架: $(cat /tmp/cc126/last.json)"

echo "== 14. 轻食接单后缺货：单品退款+临时下架 =="
BODY5='{"platform_order_no":"EM-E2E-5","store_id":1,"items":[{"sku":"LF","qty":1,"temp":"none","sugar":"无糖","cup_size":"-"},{"sku":"AM","qty":1,"temp":"hot","sugar":"无糖","cup_size":"M"}]}'
RC=$(api_code POST /v1/platform/orders 9001 "$BODY5"); expect_code 201 "轻食混合单接单"
save order5; OID5=$(getf .id order5); LFREFUND=$(getf '.cups[0].unit_price' order5)
RC=$(api_code POST "/v1/staff/orders/$OID5/item-oos" 3001 '{"sku":"LF","note":"鸡肉卷售罄"}')
expect_code 200 "轻食缺货处理"
[ "$(J .refund_amount)" = "$LFREFUND" ] && ok "轻食单品退款 $LFREFUND" || bad "退款金额 $(J .refund_amount)"
api GET /v1/stores/1/delistings 3001 > /tmp/cc126/last.json
[ "$(J '[.[]|select(.sku=="LF" and .active==true)]|length')" = "1" ] && ok "轻食临时下架" || bad "轻食未下架"
# 剩余美式做好交接
AMCUP=$(api GET "/v1/orders/$OID5" 0000 | jq -r '.cups|map(select(.status!="voided"))[0].id')
api POST /v1/staff/stores/1/cups/next 3001 '{"store_id":1}' >/dev/null
api POST "/v1/staff/cups/$AMCUP/start" 3001 >/dev/null
api POST "/v1/staff/cups/$AMCUP/seal" 3001 >/dev/null
RC=$(api_code POST /v1/rider/handoffs 4001 "{\"order_id\":$OID5,\"actual_bags\":1,\"actual_hot\":1}")
expect_code 200 "缺货后剩余商品正常交接"

echo "== 15. 爆单保护：手动暂停 + 自动容量阈值 =="
RC=$(api_code POST /v1/staff/stores/1/surge 2001 '{"paused":true,"reason":"午高峰爆单","minutes":30}')
expect_code 200 "开启爆单暂停"
RC=$(api_code POST /v1/platform/orders 9001 '{"platform_order_no":"EM-E2E-X","store_id":1,"items":[{"sku":"AM","qty":1,"temp":"hot","sugar":"无糖","cup_size":"M"}]}')
expect_code 429 "爆单期间拒单"
[ "$(J .error)" = "surge_paused" ] && ok "错误码 surge_paused" || bad "错误码 $(J .error)"
api POST /v1/staff/stores/1/surge 2001 '{"paused":false}' > /dev/null
# 容量阈值：门店2 阈值调到 1，下 2 杯单
$PSQL -c "UPDATE stores SET max_active_cups=1 WHERE id=2" >/dev/null
RC=$(api_code POST /v1/platform/orders 9002 '{"platform_order_no":"MT-E2E-CAP","store_id":2,"items":[{"sku":"AM","qty":2,"temp":"hot","sugar":"无糖","cup_size":"M"}]}')
expect_code 429 "超在制杯量阈值拒单"
[ "$(J .error)" = "surge_full" ] && ok "错误码 surge_full" || bad "错误码 $(J .error)"
$PSQL -c "UPDATE stores SET max_active_cups=12 WHERE id=2" >/dev/null

echo "== 16. 平台取消：未做回库、退款 =="
BEFORE=$($PSQL -c "SELECT qty FROM inventory WHERE store_id=1 AND ingredient_id=(SELECT id FROM ingredients WHERE sku='bean')")
BODY6='{"platform_order_no":"EM-E2E-6","store_id":1,"items":[{"sku":"AM","qty":2,"temp":"hot","sugar":"无糖","cup_size":"M"}]}'
RC=$(api_code POST /v1/platform/orders 9001 "$BODY6"); expect_code 201 "第六单接单"
save order6; OID6=$(getf .id order6)
CUP6=$(api GET "/v1/orders/$OID6" 0000 | jq -r '.cups[0].id')
api POST /v1/staff/stores/1/cups/next 3001 '{"store_id":1}' >/dev/null
api POST "/v1/staff/cups/$CUP6/start" 3001 >/dev/null   # 一杯制作中，一杯未动
RC=$(api_code POST "/v1/platform/orders/$OID6/cancel" 5001 '{"reason":"顾客取消","store_fault":false}')
expect_code 200 "平台取消成功"
[ "$(J .bearer)" = "platform" ] && ok "非门店责任，退款承担方=平台" || bad "承担方 $(J .bearer)"
AFTER=$($PSQL -c "SELECT qty FROM inventory WHERE store_id=1 AND ingredient_id=(SELECT id FROM ingredients WHERE sku='bean')")
[ "$BEFORE" = "$AFTER" ] && ok "未制作杯原料已回库 ($BEFORE)" || bad "回库异常 $BEFORE -> $AFTER"
api GET "/v1/orders/$OID6/events" 0000 > /tmp/cc126/last.json
[ "$(J '[.[]|select(.event_type=="order_canceled")]|length')" = "1" ] && ok "取消事件进订单时间线" || bad "无取消事件"

echo "== 17. 多平台订单合并 =="
RC=$(api_code POST /v1/platform/orders 9001 '{"platform_order_no":"EM-E2E-M1","store_id":2,"items":[{"sku":"AM","qty":1,"temp":"hot","sugar":"无糖","cup_size":"M"}]}')
expect_code 201 "饿了么单(门店2)"
save m1; A=$(getf .id m1)
RC=$(api_code POST /v1/platform/orders 9002 "{\"platform_order_no\":\"MT-E2E-M2\",\"store_id\":2,\"items\":[{\"sku\":\"LT\",\"qty\":1,\"temp\":\"ice\",\"sugar\":\"全糖\",\"cup_size\":\"L\"}]}")
expect_code 201 "美团单(门店2)"
save m2; B=$(getf .id m2)
RC=$(api_code POST /v1/staff/merge 3001 "{\"store_id\":2,\"order_ids\":[$A,$B],\"note\":\"同一骑手顺路单\"}")
expect_code 201 "两平台订单合并"
GID=$(J .group_id)
# 两单都做掉
for O in $A $B; do
  C=$(api GET "/v1/orders/$O" 0000 | jq -r '.cups[0].id')
  api POST "/v1/staff/cups/$C/start" 3001 >/dev/null
  api POST "/v1/staff/cups/$C/seal" 3001 >/dev/null
done
RC=$(api_code POST /v1/rider/arrivals 4002 "{\"group_id\":$GID,\"order_id\":$A}")
expect_code 200 "骑手按合并组一次到店登记"
[ "$(J '.orders|length')" = "2" ] && ok "合并组 2 单同时登记" || bad "登记单数 $(J '.orders|length')"
for O in $A $B; do
  if [ "$O" = "$A" ]; then
    # 饿了么单：骑手本人按 order_id 核验
    api POST /v1/rider/handoffs 4002 "{\"order_id\":$O,\"actual_bags\":1,\"actual_hot\":1}" >/dev/null
  else
    # 美团冰饮单：店员按平台单号代骑手核验（含骑手姓名与吸管）
    RC=$(api_code POST /v1/rider/handoffs 3001 "{\"platform_order_no\":\"MT-E2E-M2\",\"platform\":\"meituan\",\"rider_name\":\"老吴\",\"actual_bags\":1,\"actual_cold\":1,\"has_straw\":true}")
    [ "$RC" = "200" ] && ok "店员按平台单号代骑手核验交接成功" || bad "店员代核验失败: $(cat /tmp/cc126/last.json)"
  fi
done

echo "== 18. 报表 / 班组评分 / 督导复盘 =="
api GET "/v1/reports/stores/1/summary?from=2026-09-01&to=2030-12-31" 1001 > /tmp/cc126/last.json
[ "$(J '.metrics.remakes')" -ge 1 ] && ok "门店报表统计到重做" || bad "报表重做数 $(cat /tmp/cc126/last.json | head -c 300)"
[ "$(J '.metrics.missing_straw + .metrics.missing_utensil')" -ge 2 ] && ok "漏吸管/餐具入报表" || bad "漏餐具统计"
[ "$(J '.metrics.food_oos')" -ge 1 ] && ok "轻食缺货入报表" || bad "缺货统计"
api GET "/v1/reports/region?region=华东&from=2026-09-01&to=2030-12-31" 1001 > /tmp/cc126/last.json
[ "$(J '.stores|length')" = "2" ] && ok "区域经理看到 2 家门店对比" || bad "区域门店数"
api POST /v1/management/stores/1/team-score/refresh 1002 > /tmp/cc126/last.json
SCORE=$(J .score)
[ "$SCORE" -lt 100 ] && ok "班组评分因异常扣分 (<100, 实际 $SCORE)" || bad "评分未扣分 $SCORE"
api GET /v1/management/team-scores 1001 > /tmp/cc126/last.json
[ "$(J 'length')" = "1" ] && ok "班组评分榜可查" || bad "评分榜"
RC=$(api_code POST /v1/management/stores/1/reviews 1002 '{"from":"2026-09-01","to":"2030-12-31","summary":"午高峰出杯慢、漏餐具集中，加强封签核对","actions":["高峰加派1人","出杯叫号培训"]}')
expect_code 201 "督导复盘创建"
jq -e '.remake_cost_total < 0' /tmp/cc126/last.json >/dev/null && ok "重做成本计入复盘 ($(J .remake_cost_total))" || bad "复盘重做成本"

echo "== 19. 时间线串联六类角色 =="
api GET "/v1/orders/$OID/events" 0000 > /tmp/cc126/last.json
ROLES=$(J '[.[]|select(.actor_role!="")|.actor_role]|unique|join(",")')
echo "  订单1出现的角色: $ROLES"
for role in platform barista rider customer finance; do
  echo "$ROLES" | grep -q "$role" && ok "时间线含 $role" || bad "时间线缺 $role"
done

echo "== 20. 过久重做并发交接：规则引擎 / wait_new / take_original / abort / 责任复核 / 补偿兑现 =="
# 20.1 规则引擎（不重做）：热美式 15/20；冰冷萃 40/55；忙时果咖 25
RC=$(api_code POST /v1/platform/orders 9001 '{"platform_order_no":"EM-E2E-S1","store_id":1,"items":[{"sku":"AM","qty":1,"temp":"hot","sugar":"无糖","cup_size":"M"}]}')
expect_code 201 "规则单1接单"; save s1; OS1=$(getf .id s1); CS1=$(api GET "/v1/orders/$OS1" 0000 | jq -r '.cups[0].id')
api POST "/v1/staff/cups/$CS1/start" 3001 >/dev/null; api POST "/v1/staff/cups/$CS1/seal" 3001 >/dev/null
$PSQL -c "UPDATE order_cups SET sealed_at=now()-interval '16 min' WHERE id=$CS1" >/dev/null
api GET "/v1/staff/cups/$CS1/stale-check" 3001 > /tmp/cc126/last.json
[ "$(J .stale)" = "true" ] && [ "$(J .safe_minutes)" = "15" ] && [ "$(J .hard_safe_minutes)" = "20" ] && ok "热饮16分钟判定过久(15/20)" || bad "热饮规则: $(cat /tmp/cc126/last.json)"
RC=$(api_code POST /v1/platform/orders 9001 '{"platform_order_no":"EM-E2E-S2","store_id":1,"items":[{"sku":"CB","qty":1,"temp":"ice","sugar":"无糖","cup_size":"L"}]}')
expect_code 201 "规则单2接单"; save s2; OS2=$(getf .id s2); CS2=$(api GET "/v1/orders/$OS2" 0000 | jq -r '.cups[0].id')
api POST "/v1/staff/cups/$CS2/start" 3001 >/dev/null; api POST "/v1/staff/cups/$CS2/seal" 3001 >/dev/null
$PSQL -c "UPDATE order_cups SET sealed_at=now()-interval '30 min' WHERE id=$CS2" >/dev/null
api GET "/v1/staff/cups/$CS2/stale-check" 3001 > /tmp/cc126/last.json
[ "$(J .stale)" = "false" ] && [ "$(J .safe_minutes)" = "40" ] && ok "冰冷萃30分钟未过久(阈值40)" || bad "冷萃规则"
api GET "/v1/staff/cups/$CS2/stale-check?force_busy=1" 3001 > /tmp/cc126/last.json
[ "$(J .busyness)" = "busy" ] && ok "门店忙闲因子可评估" || bad "忙闲因子"

# 20.2 take_original：冰饮硬窗 35，26 分钟 + 未打新杯贴 → 骑手可取原杯，新杯作废回库，补偿作废
RC=$(api_code POST /v1/platform/orders 9001 '{"platform_order_no":"EM-E2E-T1","store_id":1,"items":[{"sku":"AM","qty":1,"temp":"ice","sugar":"无糖","cup_size":"L"}]}')
expect_code 201 "取原杯单接单"; save t1; OT1=$(getf .id t1); CT1=$(api GET "/v1/orders/$OT1" 0000 | jq -r '.cups[0].id')
api POST "/v1/staff/cups/$CT1/start" 3001 >/dev/null; api POST "/v1/staff/cups/$CT1/seal" 3001 >/dev/null
$PSQL -c "UPDATE order_cups SET sealed_at=now()-interval '26 min' WHERE id=$CT1" >/dev/null
BEAN_T1=$($PSQL -c "SELECT qty FROM inventory WHERE store_id=1 AND ingredient_id=(SELECT id FROM ingredients WHERE sku='bean')")
RC=$(api_code POST "/v1/staff/cups/$CT1/remake" 3001 '{"reason":"stale_drink","detail":{"waited_minutes":26}}')
expect_code 201 "过久重做受理（含安全窗口）"
RT1=$(J .remake_id); NT1=$($PSQL -c "SELECT new_cup_id FROM remakes WHERE id=$RT1")
[ "$($PSQL -c "SELECT status FROM order_cups WHERE id=$CT1")" = "loss_hold" ] && ok "原杯 loss_hold 不可交接" || bad "原杯状态"
api GET "/v1/rider/orders/$OT1/remake-decision" 4001 > /tmp/cc126/last.json
[ "$(J '.remakes[0].recommended')" = "take_original" ] && [ "$(J '.remakes[0].original_in_safety_window')" = "true" ] && ok "推荐 take_original（硬窗内/未打杯贴）" || bad "决策推荐: $(cat /tmp/cc126/last.json)"
RC=$(api_code POST "/v1/rider/orders/$OT1/remake-decision" 4001 "{\"remake_id\":$RT1,\"decision\":\"take_original\"}")
expect_code 200 "骑手决策取原杯"
[ "$($PSQL -c "SELECT status FROM order_cups WHERE id=$CT1")" = "sealed" ] && [ "$($PSQL -c "SELECT status FROM order_cups WHERE id=$NT1")" = "voided" ] && ok "原杯恢复可交接/新杯作废" || bad "取原杯杯状态"
[ "$($PSQL -c "SELECT qty FROM inventory WHERE store_id=1 AND ingredient_id=(SELECT id FROM ingredients WHERE sku='bean')")" = "$BEAN_T1" ] && ok "放弃新杯后预扣原料回库（全程仅消耗原杯一杯）" || bad "取原杯回库"
[ "$($PSQL -c "SELECT COALESCE(sum(amount),0) FROM finance_records WHERE order_id=$OT1 AND kind='remake_cost'")" = "0.00" ] && ok "取原杯后追加冲销，重做成本净额为 0（不覆盖原记录）" || bad "重做成本冲销"
[ "$($PSQL -c "SELECT count(*) FROM compensations WHERE remake_id=$RT1 AND status='voided'")" = "1" ] && ok "取原杯后补偿方案作废" || bad "补偿作废"
RC=$(api_code POST /v1/rider/handoffs 4001 "{\"order_id\":$OT1,\"actual_bags\":1,\"actual_cold\":1,\"has_straw\":true}")
expect_code 200 "原杯正常交接完成"

# 20.3 新杯贴打印后禁止取原杯
RC=$(api_code POST /v1/platform/orders 9001 '{"platform_order_no":"EM-E2E-T2","store_id":1,"items":[{"sku":"AM","qty":1,"temp":"ice","sugar":"无糖","cup_size":"L"}]}')
expect_code 201 "杯贴因子单接单"; save t2; OT2=$(getf .id t2); CT2=$(api GET "/v1/orders/$OT2" 0000 | jq -r '.cups[0].id')
api POST "/v1/staff/cups/$CT2/start" 3001 >/dev/null; api POST "/v1/staff/cups/$CT2/seal" 3001 >/dev/null
$PSQL -c "UPDATE order_cups SET sealed_at=now()-interval '26 min' WHERE id=$CT2" >/dev/null
RT2=$(api POST "/v1/staff/cups/$CT2/remake" 3001 '{"reason":"stale_drink"}' | jq -r .remake_id)
NT2=$($PSQL -c "SELECT new_cup_id FROM remakes WHERE id=$RT2")
api POST "/v1/staff/cups/$NT2/label-print" 3001 > /tmp/cc126/last.json
RC=$(api_code POST "/v1/rider/orders/$OT2/remake-decision" 4001 "{\"remake_id\":$RT2,\"decision\":\"take_original\"}")
[ "$RC" = "409" ] && [ "$(J .error)" = "new_label_printed" ] && ok "新杯贴已打印禁止取原杯" || bad "杯贴因子: $RC"
# 完成该单：做新杯扫新码交接
api POST "/v1/staff/cups/$NT2/start" 3001 >/dev/null
NT2SEAL=$(api POST "/v1/staff/cups/$NT2/seal" 3001 | jq -r '.cup.seal_code')
RC=$(api_code POST /v1/rider/handoffs 4001 "{\"order_id\":$OT2,\"actual_bags\":1,\"actual_cold\":1,\"has_straw\":true,\"seal_codes\":[\"$NT2SEAL\"]}")
expect_code 200 "等待新杯后扫新码交接"

# 20.4 顾客取消终止重做：新杯回库、补偿作废、流程 aborted
RC=$(api_code POST /v1/platform/orders 9001 '{"platform_order_no":"EM-E2E-T3","store_id":1,"items":[{"sku":"AM","qty":1,"temp":"ice","sugar":"无糖","cup_size":"L"}]}')
expect_code 201 "取消重做单接单"; save t3; OT3=$(getf .id t3); CT3=$(api GET "/v1/orders/$OT3" 0000 | jq -r '.cups[0].id')
api POST "/v1/staff/cups/$CT3/start" 3001 >/dev/null; api POST "/v1/staff/cups/$CT3/seal" 3001 >/dev/null
$PSQL -c "UPDATE order_cups SET sealed_at=now()-interval '26 min' WHERE id=$CT3" >/dev/null
RT3=$(api POST "/v1/staff/cups/$CT3/remake" 3001 '{"reason":"stale_drink"}' | jq -r .remake_id)
RC=$(api_code POST "/v1/platform/orders/$OT3/cancel" 5001 '{"reason":"顾客取消","store_fault":false}')
expect_code 200 "顾客取消"
[ "$($PSQL -c "SELECT status FROM remakes WHERE id=$RT3")" = "aborted" ] && ok "重做随取消 aborted" || bad "重做终止"
[ "$($PSQL -c "SELECT count(*) FROM compensations WHERE remake_id=$RT3 AND status='voided'")" = "1" ] && ok "补偿方案随取消作废" || bad "补偿随取消"
RC=$(api_code POST "/v1/rider/orders/$OT3/remake-decision" 4001 "{\"remake_id\":$RT3,\"decision\":\"abort\"}")
expect_code 200 "骑手确认终止重做"

# 20.5 责任复核：店长申请平台派单延迟 → 区域经理改判 → 财务追加冲销/重挂（不覆盖原始）
EXC_T2=$($PSQL -c "SELECT exception_id FROM remakes WHERE id=$RT2")
RC=$(api_code POST "/v1/management/exceptions/$EXC_T2/responsibility-review" 2001 '{"request_type":"platform_dispatch_delay","reason":"平台派单延迟骑手迟迟未到","evidence":{"delay_min":40}}')
expect_code 201 "店长申请责任复核"
RVID=$(J .review_id)
[ "$(J .original_responsibility)" = "store" ] && ok "记录原始责任 store" || bad "原始责任"
RC=$(api_code POST "/v1/management/exceptions/$EXC_T2/responsibility-review" 2001 '{"request_type":"platform_dispatch_delay","reason":"重复申请"}')
[ "$RC" = "409" ] && ok "重复复核被拒" || bad "重复复核 $RC"
RC=$(api_code POST "/v1/management/responsibility-reviews/$RVID/decision" 2001 '{"approve":true,"new_responsibility":"platform"}')
[ "$RC" = "403" ] && ok "店长无权改判(403)" || bad "改判权限 $RC"
RC=$(api_code POST "/v1/management/responsibility-reviews/$RVID/decision" 1001 '{"approve":true,"new_responsibility":"platform","note":"派单延迟成立改判平台"}')
expect_code 200 "区域经理改判平台"
[ "$(J .new_responsibility)" = "platform" ] && [ "$(J .reallocated_finance_records)" -ge 2 ] && ok "追加冲销+重挂财务记录" || bad "财务追加: $(cat /tmp/cc126/last.json)"
[ "$($PSQL -c "SELECT original_responsibility FROM responsibility_reviews WHERE id=$RVID")" = "store" ] && ok "原始责任不被覆盖" || bad "原始责任保留"
TEAM_NET=$($PSQL -c "SELECT COALESCE(sum(amount),0) FROM finance_records WHERE exception_id=$EXC_T2 AND kind='remake_cost' AND bearer='team'")
[ "$TEAM_NET" = "0.00" ] && ok "班组利润回算 team 净额=0" || bad "班组回算 $TEAM_NET"
# 20.6 顾客端延迟原因/补偿 + 平台客服确认补偿兑现（承担方随改判=platform）
api GET "/v1/customers/orders/$OT2/delay" 7001 > /tmp/cc126/last.json
[ "$(J '.compensations|length')" -ge 1 ] && [ "$(J '.delay_reasons|length')" -ge 1 ] && ok "顾客端展示延迟原因与补偿方案" || bad "顾客端: $(cat /tmp/cc126/last.json)"
COMP_T2=$($PSQL -c "SELECT id FROM compensations WHERE remake_id=$RT2 ORDER BY id LIMIT 1")
RC=$(api_code POST "/v1/platform/compensations/$COMP_T2/fulfill" 5001 '{"note":"优惠券已到账"}')
expect_code 200 "平台客服确认补偿兑现"
[ "$(J .status)" = "fulfilled" ] && [ "$(J .bearer)" = "platform" ] && ok "补偿已兑现且承担方随改判为平台" || bad "兑现承担方: $(cat /tmp/cc126/last.json)"
api GET "/v1/compensations?status=fulfilled" 5001 > /tmp/cc126/last.json
[ "$(J "map(select(.compensation_id==$COMP_T2))|length")" = "1" ] && ok "补偿兑现队列可查" || bad "补偿队列"
RC=$(api_code POST "/v1/platform/compensations/$COMP_T2/fulfill" 5001 '{"note":"重复"}')
[ "$RC" = "409" ] && ok "补偿不可重复兑现" || bad "重复兑现 $RC"
# 20.7 并发重做互斥（同一杯链仅一个进行中重做）
RC=$(api_code POST /v1/platform/orders 9001 '{"platform_order_no":"EM-E2E-T4","store_id":1,"items":[{"sku":"AM","qty":1,"temp":"ice","sugar":"无糖","cup_size":"L"}]}')
expect_code 201 "并发重做单接单"; save t4; OT4=$(getf .id t4); CT4=$(api GET "/v1/orders/$OT4" 0000 | jq -r '.cups[0].id')
api POST "/v1/staff/cups/$CT4/start" 3001 >/dev/null; api POST "/v1/staff/cups/$CT4/seal" 3001 >/dev/null
$PSQL -c "UPDATE order_cups SET sealed_at=now()-interval '26 min' WHERE id=$CT4" >/dev/null
( curl -s --noproxy '*' -X POST "http://localhost:8099/v1/staff/cups/$CT4/remake" -H "X-Staff-Code: 3001" -H "Content-Type: application/json" -d '{"reason":"stale_drink"}' -o /tmp/cc126/pa.json &
  curl -s --noproxy '*' -X POST "http://localhost:8099/v1/staff/cups/$CT4/remake" -H "X-Staff-Code: 3001" -H "Content-Type: application/json" -d '{"reason":"stale_drink"}' -o /tmp/cc126/pb.json & wait )
[ "$(jq -r 'select(.remake_id!=null)|1' /tmp/cc126/pa.json /tmp/cc126/pb.json | wc -l)" = "1" ] && ok "并发重做仅一次成功" || bad "并发重做: $(cat /tmp/cc126/pa.json) / $(cat /tmp/cc126/pb.json)"
[ "$($PSQL -c "SELECT count(*) FROM remakes WHERE order_id=$OT4 AND status IN ('in_progress','new_ready')")" = "1" ] && ok "同时仅一个进行中重做" || bad "重做唯一性"
# 时间线含重做/决策/复核事件
api GET "/v1/orders/$OT2/events" 0000 > /tmp/cc126/last.json
for ev in cup_remade remake_new_ready remake_completed responsibility_review_requested responsibility_review_decided compensation_fulfilled; do
  [ "$(J "[.[]|select(.event_type==\"$ev\")]|length")" -ge 1 ] && ok "时间线含 $ev" || bad "时间线缺 $ev"
done

echo
echo "================ 结果: PASS=$PASS FAIL=$FAIL ================"
[ "$FAIL" = 0 ]
