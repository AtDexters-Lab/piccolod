class TaskProgressEvent {

  const TaskProgressEvent({
    required this.taskId,
    required this.taskType,
    required this.phase,
    required this.progress,
    required this.message,
    required this.isComplete,
    this.instanceId,
    this.error,
    this.timestamp,
    this.metadata,
  });

  factory TaskProgressEvent.fromJson(Map<String, dynamic> json) {
    return TaskProgressEvent(
      taskId: (json['task_id'] as String?) ?? '',
      taskType: (json['task_type'] as String?) ?? '',
      instanceId: json['instance_id'] as String?,
      phase: (json['phase'] as String?) ?? '',
      progress: json['progress'] is int ? json['progress'] as int : -1,
      message: (json['message'] as String?) ?? '',
      isComplete: json['is_complete'] == true,
      error: json['error'] as String?,
      timestamp: json['timestamp'] != null
          ? DateTime.tryParse(json['timestamp'] as String)
          : null,
      metadata: json['metadata'] is Map
          ? Map<String, dynamic>.from(json['metadata'] as Map)
          : null,
    );
  }
  final String taskId;
  final String taskType;
  final String? instanceId;
  final String phase;
  final int progress;
  final String message;
  final bool isComplete;
  final String? error;
  final DateTime? timestamp;
  final Map<String, dynamic>? metadata;

  /// Parses image pull progress from metadata when phase is 'pulling_image'.
  ImagePullProgress? get imagePullProgress {
    if (phase != 'pulling_image' || metadata == null) return null;
    return ImagePullProgress.fromMetadata(metadata!);
  }
}

/// Progress information for an image pull operation.
class ImagePullProgress {

  const ImagePullProgress({
    this.service,
    this.image,
    this.pullPhase,
    this.overallPercent = -1,
    this.totalBytes = 0,
    this.downloadedBytes = 0,
    this.layers = const [],
    this.cached = false,
  });

  factory ImagePullProgress.fromMetadata(Map<String, dynamic> metadata) {
    final layersRaw = metadata['layers'];
    final layers = <LayerProgress>[];
    if (layersRaw is List) {
      for (final item in layersRaw) {
        if (item is Map) {
          layers.add(LayerProgress.fromMap(Map<String, dynamic>.from(item)));
        }
      }
    }

    return ImagePullProgress(
      service: metadata['service']?.toString(),
      image: metadata['image']?.toString(),
      pullPhase: metadata['phase']?.toString(),
      overallPercent: _parseInt(metadata['overall_percent']),
      totalBytes: _parseInt(metadata['total_bytes']),
      downloadedBytes: _parseInt(metadata['downloaded_bytes']),
      layers: layers,
      cached: metadata['cached'] == true,
    );
  }
  final String? service;
  final String? image;
  final String? pullPhase;
  final int overallPercent;
  final int totalBytes;
  final int downloadedBytes;
  final List<LayerProgress> layers;
  final bool cached;

  /// Returns a human-readable string for the downloaded/total bytes.
  String get progressText {
    if (totalBytes > 0) {
      return '${_formatBytes(downloadedBytes)} / ${_formatBytes(totalBytes)}';
    }
    return '';
  }

  static int _parseInt(dynamic value) {
    if (value is int) return value;
    if (value is String) return int.tryParse(value) ?? -1;
    return -1;
  }

  static String _formatBytes(int bytes) {
    if (bytes < 1024) return '$bytes B';
    if (bytes < 1024 * 1024) return '${(bytes / 1024).toStringAsFixed(1)} KB';
    if (bytes < 1024 * 1024 * 1024) {
      return '${(bytes / (1024 * 1024)).toStringAsFixed(1)} MB';
    }
    return '${(bytes / (1024 * 1024 * 1024)).toStringAsFixed(1)} GB';
  }
}

/// Progress information for a single layer being pulled.
class LayerProgress {

  const LayerProgress({
    required this.layerId,
    required this.status,
    this.bytesCurrent = 0,
    this.bytesTotal = 0,
  });

  factory LayerProgress.fromMap(Map<String, dynamic> map) {
    return LayerProgress(
      layerId: map['layer_id']?.toString() ?? '',
      status: map['status']?.toString() ?? '',
      bytesCurrent: _parseInt(map['bytes_current']),
      bytesTotal: _parseInt(map['bytes_total']),
    );
  }
  final String layerId;
  final String status;
  final int bytesCurrent;
  final int bytesTotal;

  /// Returns a short display ID (first 12 chars of the hash).
  String get shortId {
    final id = layerId;
    if (id.startsWith('sha256:')) {
      final hash = id.substring(7);
      return hash.length > 12 ? hash.substring(0, 12) : hash;
    }
    return id.length > 12 ? id.substring(0, 12) : id;
  }

  /// Returns the percentage complete, or -1 if unknown.
  int get percent {
    if (bytesTotal <= 0) return -1;
    return ((bytesCurrent * 100) / bytesTotal).round().clamp(0, 100);
  }

  static int _parseInt(dynamic value) {
    if (value is int) return value;
    if (value is String) return int.tryParse(value) ?? 0;
    return 0;
  }
}
