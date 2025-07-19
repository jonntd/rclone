#!/bin/bash

# 🔍 123网盘和115网盘代码审核脚本
echo "🔍 开始审核123网盘和115网盘代码..."

# 创建审核报告目录
mkdir -p code_audit_reports
cd code_audit_reports

echo ""
echo "📋 代码审核计划："
echo "1. 查找重复函数"
echo "2. 查找未使用的函数"
echo "3. 查找冗余的结构体字段"
echo "4. 查找可合并的相似代码"
echo "5. 查找过度开发的功能"
echo "6. 分析代码复杂度"
echo ""

# 步骤1: 查找重复函数
echo "🔧 步骤1: 查找重复函数"
echo "=== 重复函数分析 ===" > duplicate_functions.txt

echo "--- 123网盘函数列表 ---" >> duplicate_functions.txt
grep -n "^func " ../backend/123/pan123drive.go | head -20 >> duplicate_functions.txt

echo "" >> duplicate_functions.txt
echo "--- 115网盘函数列表 ---" >> duplicate_functions.txt
grep -n "^func " ../backend/115/115.go | head -20 >> duplicate_functions.txt

echo "" >> duplicate_functions.txt
echo "--- 可能重复的函数名 ---" >> duplicate_functions.txt
# 提取函数名并查找重复
(grep "^func " ../backend/123/pan123drive.go | sed 's/func \([^(]*\).*/\1/' | sort; \
 grep "^func " ../backend/115/115.go | sed 's/func \([^(]*\).*/\1/' | sort) | \
 sort | uniq -d >> duplicate_functions.txt

echo "重复函数分析完成，结果保存到 duplicate_functions.txt"

# 步骤2: 查找未使用的函数
echo ""
echo "🔧 步骤2: 查找未使用的函数"
echo "=== 未使用函数分析 ===" > unused_functions.txt

echo "--- 123网盘可能未使用的函数 ---" >> unused_functions.txt
# 查找定义但可能未被调用的函数
grep "^func " ../backend/123/pan123drive.go | while read line; do
    func_name=$(echo "$line" | sed 's/func \([^(]*\).*/\1/' | sed 's/.* //')
    if [ "$func_name" != "init" ] && [ "$func_name" != "main" ]; then
        # 检查函数是否在代码中被调用
        call_count=$(grep -c "$func_name(" ../backend/123/pan123drive.go || echo "0")
        if [ "$call_count" -le 1 ]; then
            echo "$line (调用次数: $call_count)" >> unused_functions.txt
        fi
    fi
done

echo "" >> unused_functions.txt
echo "--- 115网盘可能未使用的函数 ---" >> unused_functions.txt
grep "^func " ../backend/115/115.go | while read line; do
    func_name=$(echo "$line" | sed 's/func \([^(]*\).*/\1/' | sed 's/.* //')
    if [ "$func_name" != "init" ] && [ "$func_name" != "main" ]; then
        call_count=$(grep -c "$func_name(" ../backend/115/115.go || echo "0")
        if [ "$call_count" -le 1 ]; then
            echo "$line (调用次数: $call_count)" >> unused_functions.txt
        fi
    fi
done

echo "未使用函数分析完成，结果保存到 unused_functions.txt"

# 步骤3: 查找冗余的结构体字段
echo ""
echo "🔧 步骤3: 查找冗余的结构体字段"
echo "=== 冗余结构体字段分析 ===" > redundant_fields.txt

echo "--- 123网盘Fs结构体字段 ---" >> redundant_fields.txt
sed -n '/type Fs struct/,/^}/p' ../backend/123/pan123drive.go | grep -E "^\s+\w+" >> redundant_fields.txt

echo "" >> redundant_fields.txt
echo "--- 115网盘Fs结构体字段 ---" >> redundant_fields.txt
sed -n '/type Fs struct/,/^}/p' ../backend/115/115.go | grep -E "^\s+\w+" >> redundant_fields.txt

echo "冗余字段分析完成，结果保存到 redundant_fields.txt"

# 步骤4: 查找相似的错误处理代码
echo ""
echo "🔧 步骤4: 查找相似的错误处理代码"
echo "=== 相似错误处理分析 ===" > similar_error_handling.txt

echo "--- 123网盘错误处理模式 ---" >> similar_error_handling.txt
grep -n -A 3 -B 1 "if err != nil" ../backend/123/pan123drive.go | head -20 >> similar_error_handling.txt

echo "" >> similar_error_handling.txt
echo "--- 115网盘错误处理模式 ---" >> similar_error_handling.txt
grep -n -A 3 -B 1 "if err != nil" ../backend/115/115.go | head -20 >> similar_error_handling.txt

echo "相似错误处理分析完成，结果保存到 similar_error_handling.txt"

# 步骤5: 查找过度开发的功能
echo ""
echo "🔧 步骤5: 查找过度开发的功能"
echo "=== 过度开发功能分析 ===" > over_engineered.txt

echo "--- 复杂的管理器和组件 ---" >> over_engineered.txt
echo "123网盘管理器组件:" >> over_engineered.txt
grep -n "Manager\|Pool\|Coordinator\|Adjuster" ../backend/123/pan123drive.go >> over_engineered.txt

echo "" >> over_engineered.txt
echo "115网盘管理器组件:" >> over_engineered.txt
grep -n "Manager\|Pool\|Coordinator\|Adjuster" ../backend/115/115.go >> over_engineered.txt

echo "" >> over_engineered.txt
echo "--- 可能过度复杂的函数 ---" >> over_engineered.txt
echo "123网盘长函数名:" >> over_engineered.txt
grep "^func " ../backend/123/pan123drive.go | awk 'length($0) > 80' >> over_engineered.txt

echo "" >> over_engineered.txt
echo "115网盘长函数名:" >> over_engineered.txt
grep "^func " ../backend/115/115.go | awk 'length($0) > 80' >> over_engineered.txt

echo "过度开发功能分析完成，结果保存到 over_engineered.txt"

# 步骤6: 代码行数和复杂度统计
echo ""
echo "🔧 步骤6: 代码复杂度统计"
echo "=== 代码复杂度统计 ===" > complexity_stats.txt

echo "--- 文件大小对比 ---" >> complexity_stats.txt
echo "123网盘主文件:" >> complexity_stats.txt
wc -l ../backend/123/pan123drive.go >> complexity_stats.txt

echo "115网盘主文件:" >> complexity_stats.txt
wc -l ../backend/115/115.go >> complexity_stats.txt

echo "" >> complexity_stats.txt
echo "--- 函数数量统计 ---" >> complexity_stats.txt
echo "123网盘函数数量: $(grep -c "^func " ../backend/123/pan123drive.go)" >> complexity_stats.txt
echo "115网盘函数数量: $(grep -c "^func " ../backend/115/115.go)" >> complexity_stats.txt

echo "" >> complexity_stats.txt
echo "--- 注释密度分析 ---" >> complexity_stats.txt
echo "123网盘注释行数: $(grep -c "^\s*//" ../backend/123/pan123drive.go)" >> complexity_stats.txt
echo "115网盘注释行数: $(grep -c "^\s*//" ../backend/115/115.go)" >> complexity_stats.txt

echo "代码复杂度统计完成，结果保存到 complexity_stats.txt"

# 步骤7: 查找TODO和FIXME
echo ""
echo "🔧 步骤7: 查找待办事项和修复项"
echo "=== 待办事项分析 ===" > todo_fixme.txt

echo "--- 123网盘待办事项 ---" >> todo_fixme.txt
grep -n -i "todo\|fixme\|hack\|xxx\|注意\|🔧\|🚀\|⚠️" ../backend/123/pan123drive.go >> todo_fixme.txt

echo "" >> todo_fixme.txt
echo "--- 115网盘待办事项 ---" >> todo_fixme.txt
grep -n -i "todo\|fixme\|hack\|xxx\|注意\|🔧\|🚀\|⚠️" ../backend/115/115.go >> todo_fixme.txt

echo "待办事项分析完成，结果保存到 todo_fixme.txt"

echo ""
echo "📊 代码审核总结："
echo "✅ 重复函数分析: duplicate_functions.txt"
echo "✅ 未使用函数分析: unused_functions.txt"
echo "✅ 冗余字段分析: redundant_fields.txt"
echo "✅ 相似错误处理: similar_error_handling.txt"
echo "✅ 过度开发功能: over_engineered.txt"
echo "✅ 代码复杂度统计: complexity_stats.txt"
echo "✅ 待办事项分析: todo_fixme.txt"

echo ""
echo "📁 所有报告保存在: $(pwd)"
ls -la *.txt

echo ""
echo "🎯 代码审核完成！"
