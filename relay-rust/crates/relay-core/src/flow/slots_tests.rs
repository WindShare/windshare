use super::{Refusal, slots::DeliverySlots};

#[test]
fn allocation_failure_keeps_the_slot_pool_usable() {
    let mut slots = DeliverySlots::new(usize::MAX).unwrap();
    // This exceeds Vec's representable byte capacity without attempting an
    // actual huge allocation, exercising the recovery allocation failure path.
    assert_eq!(
        slots.ensure_slot(usize::MAX - 1),
        Err(Refusal::FlowAllocation)
    );
    assert_eq!(slots.usage().allocated_slots, 0);
    assert_eq!(slots.usage().retained_flows, 0);
    assert_eq!(slots.vacant(), Ok(0));
    assert_eq!(slots.usage().allocated_slots, 1);
}
