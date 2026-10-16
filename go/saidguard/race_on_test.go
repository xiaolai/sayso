//go:build race

package saidguard

// raceFactor scales TestBounds' time budget: the race detector slows this code about thirteenfold
// (bound-english-rescan: 1.8 s instrumented, 0.14 s plain). The scaled bound still separates the algorithms: the
// memo-table matcher it guards against took 3.2 and 5.7 s uninstrumented.
const raceFactor = 10
