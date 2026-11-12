// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/fleet_provision.go#L3
typedef Chandef {
	chan sync = [0] of {bool};
	chan enq = [0] of {bool};
	chan deq = [0] of {bool,bool};
	chan sending = [0] of {bool};
	chan rcving = [0] of {bool};
	chan closing = [0] of {bool};
	int size = 0;
	int num_msgs = 0;
	bool closed = false;
}



init { 
	chan child_FleetReadyWithoutReceipt30 = [1] of {int}->
	run FleetReadyWithoutReceipt3(child_FleetReadyWithoutReceipt30)->
	child_FleetReadyWithoutReceipt30?0->
stop_process:skip
}

proctype FleetReadyWithoutReceipt3(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousFleetReadyWithoutReceipt942 = [1] of {int}->
	chan child_AnonymousFleetReadyWithoutReceipt841 = [1] of {int}->
	chan child_AnonymousFleetReadyWithoutReceipt750 = [1] of {int}->
	Chandef ready_ch->
	Chandef receipt_ch->
	Chandef intent_ch->
	run sync_monitor(intent_ch)->
	run sync_monitor(receipt_ch)->
	run sync_monitor(ready_ch)->
	run AnonymousFleetReadyWithoutReceipt75(receipt_ch,ready_ch,intent_ch,child_AnonymousFleetReadyWithoutReceipt750)->
	run receiver(child_AnonymousFleetReadyWithoutReceipt750)->
	run AnonymousFleetReadyWithoutReceipt84(intent_ch,receipt_ch,ready_ch,child_AnonymousFleetReadyWithoutReceipt841)->
	run receiver(child_AnonymousFleetReadyWithoutReceipt841)->
	run AnonymousFleetReadyWithoutReceipt94(intent_ch,receipt_ch,ready_ch,child_AnonymousFleetReadyWithoutReceipt942)->
	run receiver(child_AnonymousFleetReadyWithoutReceipt942)->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/fleet_provision.go:10:2 */
	:: ready_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/fleet_provision.go:10:2 */->
	:: ready_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/fleet_provision.go:10:2 */ -> 
		ready_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousFleetReadyWithoutReceipt75(Chandef receipt_ch;Chandef ready_ch;Chandef intent_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/fleet_provision.go:7:12 */
	:: intent_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/fleet_provision.go:7:12 */
	:: intent_ch.sync!false -> 
		intent_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/fleet_provision.go:7:12 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousFleetReadyWithoutReceipt84(Chandef intent_ch;Chandef receipt_ch;Chandef ready_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/fleet_provision.go:8:12 */
	:: intent_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/fleet_provision.go:8:12 */->
	:: intent_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/fleet_provision.go:8:12 */ -> 
		intent_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/fleet_provision.go:8:21 */
	:: receipt_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/fleet_provision.go:8:21 */
	:: receipt_ch.sync!false -> 
		receipt_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/fleet_provision.go:8:21 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousFleetReadyWithoutReceipt94(Chandef intent_ch;Chandef receipt_ch;Chandef ready_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/fleet_provision.go:9:12 */
	:: ready_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/fleet_provision.go:9:12 */
	:: ready_ch.sync!false -> 
		ready_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/fleet_provision.go:9:12 */
	fi->
	stop_process: skip->
	child!0
}

 /* ================================================================================== */
 /* ================================================================================== */
 /* ================================================================================== */ 
proctype async_monitor(Chandef ch) {
do
:: true ->
if
:: ch.closed -> 
end: if
  :: ch.num_msgs > 0 -> // cannot send on closed channel
    end4: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.deq!true,ch.num_msgs -> 
  		 ch.num_msgs = ch.num_msgs - 1
    fi->
  :: else ->    end5: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.sync!true-> 
    fi->
  fi->
:: else ->
	if
	:: ch.num_msgs == ch.size ->
		end1: if
		  :: ch.deq!false,ch.num_msgs ->
		    ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		    ch.closed = true
		   :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	:: ch.num_msgs == 0 -> 
end2:		if
		:: ch.enq!true -> // a message has been received
			ch.num_msgs = ch.num_msgs + 1
		:: ch.closing!false -> // closing the channel
			ch.closed = true
		:: ch.rcving?false ->
 		    ch.sending!true->
		fi->
		:: else -> 
		end3: if
		  :: ch.enq!true->
		     ch.num_msgs = ch.num_msgs + 1
		  :: ch.deq!false,ch.num_msgs->
		     ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		     ch.closed = true
		  :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	fi->
fi->
od->
}

proctype sync_monitor(Chandef ch) {
do
:: true->
if
:: ch.closed ->
end: if
  :: ch.enq!false-> // cannot send on closed channel
  :: ch.closing!true -> // cannot close twice a channel
  :: ch.sending!false -> // sending state of channel (closed)
  :: ch.rcving?false->
  :: ch.sync!true-> // can always receive on a closed chan
  fi->
:: else -> 
end1: if
    :: ch.rcving?false ->
       ch.sending!true->
    :: ch.closing!false ->
      ch.closed = true
    fi->
fi->
od->
stop_process: skip
}

proctype receiver(chan c) {
c?0
}

