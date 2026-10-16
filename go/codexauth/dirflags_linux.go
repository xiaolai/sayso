package codexauth

import "golang.org/x/sys/unix"

// dirFlags open a folder of the path as a path descriptor (O_PATH), never through a link: it needs search
// permission on the folders above, not read permission on the folder itself, so a 0711 home folder does not refuse
// it. An O_PATH open never opens a FIFO, and O_DIRECTORY refuses anything but a folder; fstat and openat work on
// the descriptor.
const dirFlags = unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
