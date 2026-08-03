package app

type synchronizingScreen interface {
	Sync()
}

// presentMainFrame performs a complete physical repaint. Main dashboard frames
// are infrequent enough that correctness is more important than tcell's
// incremental-diff optimisation.
func presentMainFrame(screen synchronizingScreen) {
	screen.Sync()
}
