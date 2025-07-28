# cache-cleanup 命令使用说明

## 1. 概述

`cache-cleanup` 命令用于手动触发115和123网盘的缓存清理操作。用户可以根据需要选择不同的清理策略来优化缓存性能和磁盘使用。

## 2. 命令格式

### 115网盘
```bash
rclone backend cache-cleanup 115:remote_name [options]
```

### 123网盘
```bash
rclone backend cache-cleanup 123:remote_name [options]
```

其中 `remote_name` 是你在rclone配置中设置的远程名称。

## 3. 支持的选项

### strategy
指定缓存清理策略。使用 `-o strategy=策略名` 或 `--option strategy=策略名` 来指定。

支持以下策略：

- `size` (默认): 按创建时间排序进行清理。这是默认策略，与旧版本行为兼容。
- `lru`: 按最后访问时间排序进行清理。最近最少使用的缓存条目将被优先清理。
- `priority_lru`: 综合考虑优先级和最后访问时间排序进行清理。高优先级的缓存条目（如路径解析缓存）会被保留更长时间。
- `time`: 按创建时间排序进行清理。最早创建的缓存条目将被优先清理。
- `clear`: 清空所有缓存。这是一种快速清理所有缓存的方式。

## 4. 使用示例

### 4.1. 使用默认策略清理缓存
```bash
# 115网盘
rclone backend cache-cleanup 115:mydrive

# 123网盘
rclone backend cache-cleanup 123:mydrive
```

### 4.2. 使用LRU策略清理缓存
```bash
# 115网盘
rclone backend cache-cleanup 115:mydrive -o strategy=lru

# 123网盘
rclone backend cache-cleanup 123:mydrive -o strategy=lru
```

或者使用长选项：
```bash
# 115网盘
rclone backend cache-cleanup 115: --option strategy=lru

# 123网盘
rclone backend cache-cleanup 123: --option strategy=lru
```

### 4.3. 使用priority_lru策略清理缓存
```bash
# 115网盘
rclone backend cache-cleanup 115: -o strategy=priority_lru

# 123网盘
rclone backend cache-cleanup 123: -o strategy=priority_lru
```

### 4.4. 清空所有缓存
```bash
# 115网盘
rclone backend cache-cleanup 115: -o strategy=clear

# 123网盘
rclone backend cache-cleanup 123: -o strategy=clear
```

## 5. 输出信息

命令执行后会输出清理过程的详细信息，包括：
- 开始清理的策略
- 每个缓存实例的清理结果（成功或失败）
- 清理前后的缓存大小
- 清理掉的缓存大小（MB）
- 清理完成的总体信息

例如：
```
INFO  : 115 mydrive: 开始手动缓存清理，策略: lru
INFO  : 115 mydrive: 清理path_resolve缓存成功
INFO  : 115 mydrive: 清理dir_list缓存成功
INFO  : 115 mydrive: 清理metadata缓存成功
INFO  : 115 mydrive: 清理file_id缓存成功
INFO  : 115 mydrive: 手动缓存清理完成
```

## 6. 注意事项

1.  缓存清理是一个相对耗时的操作，尤其是在缓存数据量很大时。
2.  `clear` 策略会立即删除所有缓存数据，这可能会导致后续操作需要重新建立缓存，影响性能。
3.  `lru` 和 `priority_lru` 策略会根据访问统计信息进行智能清理，通常是比较推荐的选择。
4.  如果不指定 `strategy` 选项，默认使用 `size` 策略。
5.  请使用 `-o` 或 `--option` 来传递策略选项，而不是直接使用 `--strategy`。