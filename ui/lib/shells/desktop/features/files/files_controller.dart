import 'package:flutter/material.dart';
import 'file_system_entry.dart';

class FilesController extends ChangeNotifier {
  String _currentPath = '/';
  String get currentPath => _currentPath;

  List<FileSystemEntry> _entries = [];
  List<FileSystemEntry> get entries => List.unmodifiable(_entries);

  bool _isLoading = false;
  bool get isLoading => _isLoading;

  String? _error;
  String? get error => _error;

  // History for back/forward navigation
  final List<String> _history = ['/'];
  int _historyIndex = 0;

  bool get canGoBack => _historyIndex > 0;
  bool get canGoForward => _historyIndex < _history.length - 1;

  bool _disposed = false;

  FilesController() {
    _loadPath('/');
  }

  @override
  void dispose() {
    _disposed = true;
    super.dispose();
  }

  void navigateTo(String path) {
    if (path == _currentPath) return;

    // If navigating to a new path (not back/forward), truncate future history
    if (_historyIndex < _history.length - 1) {
      _history.removeRange(_historyIndex + 1, _history.length);
    }
    
    _history.add(path);
    _historyIndex++;
    _loadPath(path);
  }

  void goBack() {
    if (canGoBack) {
      _historyIndex--;
      _loadPath(_history[_historyIndex]);
    }
  }

  void goForward() {
    if (canGoForward) {
      _historyIndex++;
      _loadPath(_history[_historyIndex]);
    }
  }

  void goUp() {
    if (_currentPath == '/') return;
    final parent = _getParentPath(_currentPath);
    navigateTo(parent);
  }

  String _getParentPath(String path) {
    final parts = path.split('/').where((s) => s.isNotEmpty).toList();
    if (parts.isEmpty) return '/';
    parts.removeLast();
    if (parts.isEmpty) return '/';
    return '/${parts.join('/')}';
  }

  Future<void> _loadPath(String path) async {
    if (_disposed) return;
    _isLoading = true;
    _error = null;
    _currentPath = path;
    notifyListeners();

    // Simulate network delay
    await Future.delayed(const Duration(milliseconds: 300));
    if (_disposed) return;

    try {
      _entries = _mockFileSystem[path] ?? [];
      // sort directories first, then files
      _entries.sort((a, b) {
        if (a.isDirectory && !b.isDirectory) return -1;
        if (!a.isDirectory && b.isDirectory) return 1;
        return a.name.compareTo(b.name);
      });
    } catch (e) {
      _error = "Failed to load directory: $path";
    } finally {
      if (!_disposed) {
        _isLoading = false;
        notifyListeners();
      }
    }
  }

  // MOCK DATA
  final Map<String, List<FileSystemEntry>> _mockFileSystem = {
    '/': [
      FileSystemEntry(name: 'home', path: '/home', isDirectory: true, size: 0, modified: DateTime.now()),
      FileSystemEntry(name: 'media', path: '/media', isDirectory: true, size: 0, modified: DateTime.now()),
      FileSystemEntry(name: 'var', path: '/var', isDirectory: true, size: 0, modified: DateTime.now()),
      FileSystemEntry(name: 'etc', path: '/etc', isDirectory: true, size: 0, modified: DateTime.now()),
    ],
    '/home': [
      FileSystemEntry(name: 'admin', path: '/home/admin', isDirectory: true, size: 0, modified: DateTime.now()),
    ],
    '/home/admin': [
      FileSystemEntry(name: 'Documents', path: '/home/admin/Documents', isDirectory: true, size: 0, modified: DateTime.now()),
      FileSystemEntry(name: 'Downloads', path: '/home/admin/Downloads', isDirectory: true, size: 0, modified: DateTime.now()),
      FileSystemEntry(name: 'Pictures', path: '/home/admin/Pictures', isDirectory: true, size: 0, modified: DateTime.now()),
      FileSystemEntry(name: 'todo.txt', path: '/home/admin/todo.txt', isDirectory: false, size: 1024, modified: DateTime.now()),
    ],
    '/home/admin/Documents': [
      FileSystemEntry(name: 'Project_Piccolo.pdf', path: '/home/admin/Documents/Project_Piccolo.pdf', isDirectory: false, size: 204800, modified: DateTime.now()),
    ],
    '/media': [],
  };
}
