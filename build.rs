fn main() {
    // Compiled into the binary: the image is FROM scratch with a read-only
    // root, so there is nothing to read them from at runtime.
    memory_serve::load_directory("./static");
}
