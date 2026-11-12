// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go#L3
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
	chan child_RetentionScheduleDropsDurableClaim30 = [1] of {int}->
	run RetentionScheduleDropsDurableClaim3(child_RetentionScheduleDropsDurableClaim30)->
	child_RetentionScheduleDropsDurableClaim30?0->
stop_process:skip
}

proctype RetentionScheduleDropsDurableClaim3(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousRetentionScheduleDropsDurableClaim1143 = [1] of {int}->
	chan child_AnonymousRetentionScheduleDropsDurableClaim1072 = [1] of {int}->
	chan child_AnonymousRetentionScheduleDropsDurableClaim941 = [1] of {int}->
	chan child_AnonymousRetentionScheduleDropsDurableClaim840 = [1] of {int}->
	Chandef pruned_ch->
	Chandef planned_ch->
	Chandef claimed_ch->
	Chandef authorized_ch->
	run sync_monitor(authorized_ch)->
	run sync_monitor(claimed_ch)->
	run sync_monitor(planned_ch)->
	run sync_monitor(pruned_ch)->
	run AnonymousRetentionScheduleDropsDurableClaim84(authorized_ch,claimed_ch,planned_ch,pruned_ch,child_AnonymousRetentionScheduleDropsDurableClaim840)->
	run receiver(child_AnonymousRetentionScheduleDropsDurableClaim840)->
	run AnonymousRetentionScheduleDropsDurableClaim94(authorized_ch,claimed_ch,planned_ch,pruned_ch,child_AnonymousRetentionScheduleDropsDurableClaim941)->
	run receiver(child_AnonymousRetentionScheduleDropsDurableClaim941)->
	run AnonymousRetentionScheduleDropsDurableClaim107(pruned_ch,authorized_ch,claimed_ch,planned_ch,child_AnonymousRetentionScheduleDropsDurableClaim1072)->
	run receiver(child_AnonymousRetentionScheduleDropsDurableClaim1072)->
	run AnonymousRetentionScheduleDropsDurableClaim114(authorized_ch,claimed_ch,planned_ch,pruned_ch,child_AnonymousRetentionScheduleDropsDurableClaim1143)->
	run receiver(child_AnonymousRetentionScheduleDropsDurableClaim1143)->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:12:2 */
	:: pruned_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:12:2 */->
	:: pruned_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:12:2 */ -> 
		pruned_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousRetentionScheduleDropsDurableClaim84(Chandef authorized_ch;Chandef claimed_ch;Chandef planned_ch;Chandef pruned_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:8:12 */
	:: authorized_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:8:12 */
	:: authorized_ch.sync!false -> 
		authorized_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:8:12 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousRetentionScheduleDropsDurableClaim94(Chandef authorized_ch;Chandef claimed_ch;Chandef planned_ch;Chandef pruned_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:9:12 */
	:: authorized_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:9:12 */->
	:: authorized_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:9:12 */ -> 
		authorized_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:9:26 */
	:: claimed_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:9:26 */
	:: claimed_ch.sync!false -> 
		claimed_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:9:26 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousRetentionScheduleDropsDurableClaim107(Chandef pruned_ch;Chandef authorized_ch;Chandef claimed_ch;Chandef planned_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:10:12 */
	:: planned_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:10:12 */
	:: planned_ch.sync!false -> 
		planned_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:10:12 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousRetentionScheduleDropsDurableClaim114(Chandef authorized_ch;Chandef claimed_ch;Chandef planned_ch;Chandef pruned_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:11:12 */
	:: planned_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:11:12 */->
	:: planned_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:11:12 */ -> 
		planned_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:11:23 */
	:: pruned_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:11:23 */
	:: pruned_ch.sync!false -> 
		pruned_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/retention_schedule.go:11:23 */
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

