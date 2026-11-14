use gpui::{
    AnyElement, App, Bounds, Element, ElementId, GlobalElementId, InspectorElementId, IntoElement,
    LayoutId, Pixels, Window,
};

/// Reserve content's natural size without painting or registering input and accessibility.
pub fn reserve_layout(content: impl IntoElement) -> impl IntoElement {
    LayoutReservation {
        content: content.into_any_element(),
    }
}

struct LayoutReservation {
    content: AnyElement,
}

impl IntoElement for LayoutReservation {
    type Element = Self;

    fn into_element(self) -> Self {
        self
    }
}

impl Element for LayoutReservation {
    type RequestLayoutState = ();
    type PrepaintState = ();

    fn id(&self) -> Option<ElementId> {
        None
    }

    fn source_location(&self) -> Option<&'static std::panic::Location<'static>> {
        None
    }

    fn request_layout(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&InspectorElementId>,
        window: &mut Window,
        cx: &mut App,
    ) -> (LayoutId, ()) {
        (self.content.request_layout(window, cx), ())
    }

    // GPUI's visibility style only skips paint. Skipping prepaint as well keeps
    // unrevealed controls out of hit testing, focus traversal and accessibility.
    fn prepaint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&InspectorElementId>,
        _: Bounds<Pixels>,
        (): &mut (),
        _: &mut Window,
        _: &mut App,
    ) {
    }

    fn paint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&InspectorElementId>,
        _: Bounds<Pixels>,
        (): &mut (),
        (): &mut (),
        _: &mut Window,
        _: &mut App,
    ) {
    }
}
