package codexauth

import "golang.org/x/sys/unix"

// oSearch is Darwin's O_SEARCH, O_EXEC | O_DIRECTORY in sys/fcntl.h, which x/sys/unix does not export.
const oSearch = 0x40000000 | unix.O_DIRECTORY

// dirFlags open a folder of the path for search only, never through a link, and never blocking on a FIFO named like
// a folder. Walking a path needs search permission on its folders, not read permission, so an ancestor such as a
// 0711 home folder does not refuse it.
const dirFlags = oSearch | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
