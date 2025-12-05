class FileSystemEntry {
  final String name;
  final String path;
  final bool isDirectory;
  final int size;
  final DateTime modified;

  const FileSystemEntry({
    required this.name,
    required this.path,
    required this.isDirectory,
    required this.size,
    required this.modified,
  });
}
