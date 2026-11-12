use std::{
    fs::{self, File, OpenOptions},
    io::{self, Write},
    path::PathBuf,
    time::SystemTime,
};

const MAXIMUM_FILE_BYTES: usize = 4 * 1024 * 1024;
const RETAINED_FILES: usize = 8;

pub struct Segments {
    directory: PathBuf,
    file: Option<File>,
    bytes: usize,
    limit: usize,
    sequence: u64,
}

impl Segments {
    pub fn open(directory: PathBuf) -> io::Result<Self> {
        fs::create_dir_all(&directory)?;
        let mut segments = Self {
            directory,
            file: None,
            bytes: 0,
            limit: MAXIMUM_FILE_BYTES,
            sequence: 0,
        };
        segments.rotate()?;
        Ok(segments)
    }

    fn rotate(&mut self) -> io::Result<()> {
        if let Some(mut file) = self.file.take() {
            file.flush()?;
        }
        let mut files = fs::read_dir(&self.directory)?
            .filter_map(Result::ok)
            .filter(|entry| {
                let name = entry.file_name();
                let name = name.to_string_lossy();
                (name.starts_with("desktop-") || name.starts_with("desktop."))
                    && name.ends_with(".log")
            })
            .filter_map(|entry| {
                entry
                    .metadata()
                    .ok()
                    .filter(std::fs::Metadata::is_file)
                    .and_then(|metadata| {
                        metadata
                            .modified()
                            .ok()
                            .map(|modified| (modified, entry.path()))
                    })
            })
            .collect::<Vec<_>>();
        files.sort();
        let remove = files.len().saturating_sub(RETAINED_FILES - 1);
        for (_, path) in files.into_iter().take(remove) {
            // A concurrent Windows writer may still own a segment; its own size
            // bound remains in force and a later rotation retries retention.
            let _ = fs::remove_file(path);
        }
        self.sequence += 1;
        let stamp = SystemTime::now()
            .duration_since(SystemTime::UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos();
        let name = format!(
            "desktop-{stamp}-{}-{}.log",
            std::process::id(),
            self.sequence
        );
        self.file = Some(
            OpenOptions::new()
                .write(true)
                .create_new(true)
                .open(self.directory.join(name))?,
        );
        self.bytes = 0;
        Ok(())
    }
}

impl Write for Segments {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        if self.bytes == self.limit {
            self.rotate()?;
        }
        let count = bytes.len().min(self.limit - self.bytes);
        let written = self
            .file
            .as_mut()
            .ok_or_else(|| io::Error::other("log segment unavailable"))?
            .write(&bytes[..count])?;
        self.bytes += written;
        Ok(written)
    }

    fn flush(&mut self) -> io::Result<()> {
        self.file.as_mut().map_or(Ok(()), Write::flush)
    }
}

#[cfg(test)]
#[path = "segments_tests.rs"]
mod tests;
