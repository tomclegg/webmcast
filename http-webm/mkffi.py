import cffi
import subprocess


ffi = cffi.FFI()
ffi.set_source('webm_stream.c', '#include <broadcast.h>', libraries=['broadcast', 'stdc++'],
    library_dirs=['./obj'],
    include_dirs=['./src'])
ffi.cdef(
    subprocess.check_output(['cpp', '-I./src', '-xc++', '-std=c++11', '-P'], input=b'''
        #include <broadcast.h>
        extern "Python" int webm_on_write(void *, const uint8_t *, size_t);
    ''').decode('utf-8')
)
ffi.compile()
