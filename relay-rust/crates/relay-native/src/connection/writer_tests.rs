use super::*;

impl Writer {
    pub(crate) async fn recv(&mut self) -> Option<Vec<u8>> {
        self.ordered.recv().await.map(|frame| frame.bytes)
    }

    pub(crate) fn try_recv(&mut self) -> Result<Vec<u8>, mpsc::error::TryRecvError> {
        self.ordered.try_recv().map(|frame| frame.bytes)
    }
}
