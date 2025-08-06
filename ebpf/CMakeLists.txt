cmake_minimum_required(VERSION 3.10)
project(ebpf C)

set(CMAKE_C_STANDARD 11)

add_executable(ebpf
        src/xdp_sipbench.c)
