// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go#L4
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
	chan child_TicketOutboxRecovery40 = [1] of {int}->
	run TicketOutboxRecovery4(child_TicketOutboxRecovery40)->
	child_TicketOutboxRecovery40?0->
stop_process:skip
}

proctype TicketOutboxRecovery4(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousTicketOutboxRecovery1454 = [1] of {int}->
	chan child_AnonymousTicketOutboxRecovery1373 = [1] of {int}->
	chan child_AnonymousTicketOutboxRecovery1252 = [1] of {int}->
	chan child_AnonymousTicketOutboxRecovery1151 = [1] of {int}->
	chan child_AnonymousTicketOutboxRecovery1080 = [1] of {int}->
	Chandef emitted_ch->
	Chandef query_ch->
	Chandef performed_ch->
	Chandef started_ch->
	Chandef ordered_ch->
	run sync_monitor(ordered_ch)->
	run sync_monitor(started_ch)->
	run sync_monitor(performed_ch)->
	run sync_monitor(query_ch)->
	run sync_monitor(emitted_ch)->
	run AnonymousTicketOutboxRecovery108(query_ch,emitted_ch,ordered_ch,started_ch,performed_ch,child_AnonymousTicketOutboxRecovery1080)->
	run receiver(child_AnonymousTicketOutboxRecovery1080)->
	run AnonymousTicketOutboxRecovery115(ordered_ch,started_ch,performed_ch,query_ch,emitted_ch,child_AnonymousTicketOutboxRecovery1151)->
	run receiver(child_AnonymousTicketOutboxRecovery1151)->
	run AnonymousTicketOutboxRecovery125(ordered_ch,started_ch,performed_ch,query_ch,emitted_ch,child_AnonymousTicketOutboxRecovery1252)->
	run receiver(child_AnonymousTicketOutboxRecovery1252)->
	run AnonymousTicketOutboxRecovery137(performed_ch,query_ch,emitted_ch,ordered_ch,started_ch,child_AnonymousTicketOutboxRecovery1373)->
	run receiver(child_AnonymousTicketOutboxRecovery1373)->
	run AnonymousTicketOutboxRecovery145(ordered_ch,started_ch,performed_ch,query_ch,emitted_ch,child_AnonymousTicketOutboxRecovery1454)->
	run receiver(child_AnonymousTicketOutboxRecovery1454)->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:15:2 */
	:: emitted_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:15:2 */->
	:: emitted_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:15:2 */ -> 
		emitted_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousTicketOutboxRecovery108(Chandef query_ch;Chandef emitted_ch;Chandef ordered_ch;Chandef started_ch;Chandef performed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:10:14 */
	:: ordered_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:10:14 */
	:: ordered_ch.sync!false -> 
		ordered_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:10:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousTicketOutboxRecovery115(Chandef ordered_ch;Chandef started_ch;Chandef performed_ch;Chandef query_ch;Chandef emitted_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:11:14 */
	:: ordered_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:11:14 */->
	:: ordered_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:11:14 */ -> 
		ordered_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:11:25 */
	:: started_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:11:25 */
	:: started_ch.sync!false -> 
		started_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:11:25 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousTicketOutboxRecovery125(Chandef ordered_ch;Chandef started_ch;Chandef performed_ch;Chandef query_ch;Chandef emitted_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:12:14 */
	:: started_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:12:14 */->
	:: started_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:12:14 */ -> 
		started_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:12:25 */
	:: performed_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:12:25 */
	:: performed_ch.sync!false -> 
		performed_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:12:25 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousTicketOutboxRecovery137(Chandef performed_ch;Chandef query_ch;Chandef emitted_ch;Chandef ordered_ch;Chandef started_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:13:14 */
	:: performed_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:13:14 */->
	:: performed_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:13:14 */ -> 
		performed_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:13:27 */
	:: query_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:13:27 */
	:: query_ch.sync!false -> 
		query_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:13:27 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousTicketOutboxRecovery145(Chandef ordered_ch;Chandef started_ch;Chandef performed_ch;Chandef query_ch;Chandef emitted_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:14:14 */
	:: query_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:14:14 */->
	:: query_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:14:14 */ -> 
		query_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:14:23 */
	:: emitted_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:14:23 */
	:: emitted_ch.sync!false -> 
		emitted_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/ticket.go:14:23 */
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

