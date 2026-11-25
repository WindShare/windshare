use std::{
    fs::{self, File, OpenOptions},
    io,
    path::Path,
};

// Match Go statefile and diaglog owner-only permissions on shared Unix config
// and cache directories so creation order never weakens directory privacy.
#[cfg(unix)]
const PRIVATE_DIRECTORY_MODE: u32 = 0o700;
#[cfg(unix)]
const PRIVATE_FILE_MODE: u32 = 0o600;

pub fn create_dir_all(path: &Path) -> io::Result<()> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;

        let mut builder = fs::DirBuilder::new();
        // Only newly created directories receive 0o700; existing ancestors and
        // targets keep their configured permissions and never trigger chmod EPERM.
        builder.recursive(true).mode(PRIVATE_DIRECTORY_MODE);
        builder.create(path)
    }
    #[cfg(not(unix))]
    {
        fs::create_dir_all(path)
    }
}

pub fn create_new_file(path: &Path) -> io::Result<File> {
    let mut options = OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;

        options.mode(PRIVATE_FILE_MODE);
    }
    options.open(path)
}

#[cfg(test)]
#[path = "private_fs_tests.rs"]
mod tests;
