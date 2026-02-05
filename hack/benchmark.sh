#!/bin/bash
# bench.sh
# etcd-benchmark 性能测试脚本
# 测试项目: txn-put, put, range, lease-keepalive

set -e

# 配置参数
ENDPOINT=${1:-"https://127.0.0.1:2479"}
CA_CERT=${CA_CERT:-"/run/etcd/ssl/ca.pem"}
CLIENT_CERT=${CLIENT_CERT:-"/run/etcd/ssl/etcd-server-0.pem"}
CLIENT_KEY=${CLIENT_KEY:-"/run/etcd/ssl/etcd-server-0-key.pem"}
OUTPUT_DIR=${OUTPUT_DIR:-"./bench_results"}
CLIENTS=${CLIENTS:-30}
CONNS=${CONNS:-5}
TOTAL=${TOTAL:-5000}

# 定义不同大小级别的测试参数
declare -A SIZES=(
    ["small"]="64:128"
    ["medium"]="256:1024"
    ["large"]="1024:8192"
)

# 证书验证函数
validate_certificates() {
    echo "验证证书文件..."

    if [[ ! -f "$CA_CERT" ]]; then
        echo "错误: CA 证书不存在: $CA_CERT"
        exit 1
    fi

    if [[ ! -f "$CLIENT_CERT" ]]; then
        echo "错误: 客户端证书不存在: $CLIENT_CERT"
        exit 1
    fi

    if [[ ! -f "$CLIENT_KEY" ]]; then
        echo "错误: 客户端私钥不存在: $CLIENT_KEY"
        exit 1
    fi

    echo "证书验证通过"
}

# 测试连接
test_connection() {
    echo "测试 etcd 连接..."

    if etcdctl --endpoints="$ENDPOINT" \
        --cacert="$CA_CERT" \
        --cert="$CLIENT_CERT" \
        --key="$CLIENT_KEY" \
        endpoint health; then
        echo "✓ 连接成功"
        return 0
    else
        echo "✗ 连接失败"
        return 1
    fi
}

# 获取 etcd 集群信息
get_cluster_info() {
    echo "获取 etcd 集群信息..."

    # 获取成员列表
    local member_list=$(etcdctl --endpoints="$ENDPOINT" \
        --cacert="$CA_CERT" \
        --cert="$CLIENT_CERT" \
        --key="$CLIENT_KEY" \
        member list -w table 2>/dev/null)

    # 获取端点状态
    local endpoint_status=$(etcdctl --endpoints="$ENDPOINT" \
        --cacert="$CA_CERT" \
        --cert="$CLIENT_CERT" \
        --key="$CLIENT_KEY" \
        endpoint status -w table 2>/dev/null)

    # 保存到临时文件供报告使用
    echo "$member_list" > "$OUTPUT_DIR/cluster_members.txt"
    echo "$endpoint_status" > "$OUTPUT_DIR/cluster_status.txt"

    # 计算节点数量
    local node_count=$(echo "$member_list" | grep -c "started" || echo "unknown")

    echo "集群节点数: $node_count"
    echo "$member_list"
    echo ""
    echo "$endpoint_status"

    return 0
}

# 通用 benchmark 函数
run_benchmark() {
    local operation=$1
    shift
    local extra_args="$*"

    echo "运行 benchmark: $operation $extra_args"

    benchmark \
        --endpoints="$ENDPOINT" \
        --cacert="$CA_CERT" \
        --cert="$CLIENT_CERT" \
        --key="$CLIENT_KEY" \
        --target-leader \
        --conns="$CONNS" \
        --clients="$CLIENTS" \
        "$operation" \
        $extra_args
}

# 1. PUT 测试
test_put() {
    echo "=== PUT 性能测试 ==="

    for size_name in "${!SIZES[@]}"; do
        IFS=':' read -r key_size val_size <<< "${SIZES[$size_name]}"

        local output_file="$OUTPUT_DIR/put_${size_name}.log"

        echo "测试 PUT 操作 - $size_name (key: ${key_size}B, value: ${val_size}B, total: $TOTAL)..."

        run_benchmark "put" \
            --key-size="$key_size" \
            --val-size="$val_size" \
            --sequential-keys \
            --key-space-size="$TOTAL" \
            --total="$TOTAL" \
            2>&1 | tee "$output_file"

        echo "PUT $size_name 测试完成，结果保存到: $output_file"
        sleep 1
    done
}

# 2. RANGE 测试
test_range() {
    echo "=== RANGE 性能测试 ==="

    for size_name in "${!SIZES[@]}"; do
        IFS=':' read -r key_size val_size <<< "${SIZES[$size_name]}"

        local output_file="$OUTPUT_DIR/range_${size_name}.log"

        # 先写入一些测试数据
        echo "准备测试数据 - $size_name (key: ${key_size}B, value: ${val_size}B)..."
        for i in {1..100}; do
            local key=$(printf "/bench/range/%0${key_size}d" $i)
            local value=$(head -c "$val_size" /dev/urandom | base64 | head -c "$val_size")
            etcdctl --endpoints="$ENDPOINT" \
                --cacert="$CA_CERT" \
                --cert="$CLIENT_CERT" \
                --key="$CLIENT_KEY" \
                put "$key" "$value" >/dev/null 2>&1
        done

        echo "测试 RANGE 操作 - $size_name (total: $TOTAL)..."

        run_benchmark "range" \
            "/bench/range/" "/bench/range/z" \
            --consistency="l" \
            --total="$TOTAL" \
            2>&1 | tee "$output_file"

        # 清理测试数据
        echo "清理测试数据..."
        etcdctl --endpoints="$ENDPOINT" \
            --cacert="$CA_CERT" \
            --cert="$CLIENT_CERT" \
            --key="$CLIENT_KEY" \
            del --prefix "/bench/range/" >/dev/null 2>&1

        echo "RANGE $size_name 测试完成，结果保存到: $output_file"
        sleep 1
    done
}

# 3. TXN-PUT 测试
test_txn_put() {
    echo "=== TXN-PUT 性能测试 ==="

    for size_name in "${!SIZES[@]}"; do
        IFS=':' read -r key_size val_size <<< "${SIZES[$size_name]}"

        local output_file="$OUTPUT_DIR/txn_put_${size_name}.log"
        local txn_ops=3
        local key_space_size=$((TOTAL * txn_ops))

        echo "测试 TXN-PUT 操作 - $size_name (key: ${key_size}B, value: ${val_size}B, txn-ops: $txn_ops, total: $TOTAL)..."

        run_benchmark "txn-put" \
            --key-size="$key_size" \
            --val-size="$val_size" \
            --txn-ops="$txn_ops" \
            --key-space-size="$key_space_size" \
            --total="$TOTAL" \
            2>&1 | tee "$output_file"

        echo "TXN-PUT $size_name 测试完成，结果保存到: $output_file"
        sleep 1
    done
}

# 4. LEASE-KEEPALIVE 测试
test_lease_keepalive() {
    echo "=== LEASE-KEEPALIVE 性能测试 ==="

    # 定义不同级别的请求总数
    declare -A LEASE_TOTALS=(
        ["small"]="3000"
        ["medium"]="5000"
        ["large"]="10000"
    )

    for size_name in "${!LEASE_TOTALS[@]}"; do
        local total="${LEASE_TOTALS[$size_name]}"
        local output_file="$OUTPUT_DIR/lease_keepalive_${size_name}.log"

        echo "测试 LEASE-KEEPALIVE 操作 - $size_name (total: $total)..."

        run_benchmark "lease-keepalive" \
            --total="$total" \
            2>&1 | tee "$output_file"

        echo "LEASE-KEEPALIVE $size_name 测试完成，结果保存到: $output_file"
        sleep 1
    done
}

# 生成测试报告
generate_report() {
    echo "=== 生成测试报告 ==="

    local report_file="$OUTPUT_DIR/benchmark_report.md"

    cat > "$report_file" << EOF
# etcd-benchmark 性能测试报告

## 测试配置
- 测试时间: $(date)
- etcd 端点: $ENDPOINT
- 客户端数: $CLIENTS
- 连接数: $CONNS
- 基准请求数: $TOTAL

## 证书配置
- CA 证书: $CA_CERT
- 客户端证书: $CLIENT_CERT
- 客户端私钥: $CLIENT_KEY

## etcd 集群信息

### 集群成员
\`\`\`
$(cat "$OUTPUT_DIR/cluster_members.txt" 2>/dev/null || echo "无法获取集群成员信息")
\`\`\`

### 端点状态
\`\`\`
$(cat "$OUTPUT_DIR/cluster_status.txt" 2>/dev/null || echo "无法获取端点状态信息")
\`\`\`

## 测试级别说明
- **small**: key=64B, value=128B
- **medium**: key=256B, value=1024B
- **large**: key=1024B, value=8192B

## 测试结果

EOF

    # 按操作类型分组汇总结果
    for operation in "put" "range" "txn_put" "lease_keepalive"; do
        echo "### ${operation^^}" >> "$report_file"
        echo "" >> "$report_file"

        for size in "small" "medium" "large"; do
            local test_file="$OUTPUT_DIR/${operation}_${size}.log"
            if [ -f "$test_file" ]; then
                echo "#### ${size^}" >> "$report_file"
                echo '```' >> "$report_file"
                grep -E "(Summary|Total|Average|Requests/sec|Slowest|Fastest|Stddev|Latency distribution)" "$test_file" | head -20 >> "$report_file" 2>/dev/null || echo "无结果数据" >> "$report_file"
                echo '```' >> "$report_file"
                echo "" >> "$report_file"
            fi
        done
    done

    echo "报告已生成: $report_file"
}

# 主函数
main() {
    echo "开始 etcd-benchmark 性能测试"
    echo "=============================="
    echo "测试项目: txn-put, put, range, lease-keepalive"
    echo ""

    mkdir -p "$OUTPUT_DIR"

    # 验证证书
    validate_certificates

    # 测试连接
    if ! test_connection; then
        echo "连接测试失败，请检查证书和端点配置"
        exit 1
    fi

    echo ""

    # 获取集群信息
    get_cluster_info

    echo ""
    echo "开始执行性能测试..."
    echo ""

    # 运行各项测试
    test_put
    echo ""
    sleep 2

    test_range
    echo ""
    sleep 2

    test_txn_put
    echo ""
    sleep 2

    test_lease_keepalive
    echo ""
    sleep 2

    # 生成报告
    generate_report

    echo ""
    echo "=============================="
    echo "性能测试完成！"
    echo "结果保存在: $OUTPUT_DIR"
    echo "测试报告: $OUTPUT_DIR/benchmark_report.md"
}

# 执行主函数
main "$@"
