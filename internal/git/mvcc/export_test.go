package mvcc

// SetBeforePointerLock installs a function that LocalStorage.Commit runs after it
// opened the manifest pointer writer and before it locks it.
func (l *LocalStorage) SetBeforePointerLock(fn func()) {
	l.beforePointerLock = fn
}
